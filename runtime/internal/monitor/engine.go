package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

type Engine struct {
	mu            sync.Mutex
	Store         *Store
	Roots         []Root
	Stale         time.Duration
	sources       map[string]*Source
	RootHealth    map[string]string
	WatcherHealth string
}

func DefaultRoots() []Root {
	home, _ := os.UserHomeDir()
	base := func(key, fallback string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return fallback
	}
	return []Root{
		{"codex", filepath.Join(base("CODEX_HOME", filepath.Join(home, ".codex")), "sessions")},
		{"claude", filepath.Join(base("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude")), "projects")},
		{"deepseek", filepath.Join(base("DSH_HOME", filepath.Join(home, ".dsh")), "sessions")},
		{"pi", base("PI_CODING_AGENT_SESSION_DIR", filepath.Join(home, ".pi", "agent", "sessions"))},
	}
}

func NewEngine(store *Store, roots []Root, stale time.Duration) (*Engine, error) {
	if stale <= 0 {
		return nil, errors.New("stale duration must be positive")
	}
	sources, err := store.Load()
	if err != nil {
		return nil, err
	}
	for i := range roots {
		switch roots[i].Agent {
		case "codex", "claude", "deepseek", "pi":
		default:
			return nil, errors.New("unsupported root agent")
		}
		roots[i].Path, err = filepath.Abs(roots[i].Path)
		if err != nil {
			return nil, err
		}
		if real, err := filepath.EvalSymlinks(roots[i].Path); err == nil {
			roots[i].Path = real
		}
	}
	for _, src := range sources {
		if info, err := os.Stat(src.Path); err == nil {
			src.Bootstrap = info.Size()
		}
	}
	return &Engine{Store: store, Roots: roots, Stale: stale, sources: sources, RootHealth: map[string]string{}, WatcherHealth: "polling"}, nil
}

type candidate struct {
	root       Root
	path       string
	generation int
	ambiguous  bool
}

var generationPattern = regexp.MustCompile(`^session(?:\.v([1-9][0-9]*))?\.jsonl(?:\.zstd)?$`)

func (e *Engine) discover(watch func(string)) []candidate {
	all := []candidate{}
	for _, root := range e.Roots {
		health := "healthy"
		byDir := map[string][]candidate{}
		err := filepath.WalkDir(root.Path, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				health = "unavailable"
				return nil
			}
			if d.IsDir() {
				if watch != nil {
					watch(path)
				}
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if root.Agent == "deepseek" {
				m := generationPattern.FindStringSubmatch(d.Name())
				if m == nil {
					return nil
				}
				n := 0
				if m[1] != "" {
					var err error
					n, err = strconv.Atoi(m[1])
					if err != nil {
						health = "invalid_generation"
						return nil
					}
				}
				c := candidate{root: root, path: path, generation: n}
				dir := filepath.Dir(path)
				byDir[dir] = append(byDir[dir], c)
			} else if strings.HasSuffix(d.Name(), ".jsonl") {
				all = append(all, candidate{root: root, path: path})
			}
			return nil
		})
		if err != nil {
			health = "unavailable"
		}
		e.RootHealth[root.Agent+":"+root.Path] = health
		for _, candidates := range byDir {
			sort.Slice(candidates, func(i, j int) bool { return candidates[i].generation > candidates[j].generation })
			c := candidates[0]
			if len(candidates) > 1 && candidates[1].generation == c.generation {
				c.ambiguous = true
			}
			all = append(all, c)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].path < all[j].path })
	return all
}

// Scan commits each source together with its normalized event batch. A parser
// failure affects only that source; storage failures stop the scan.
func (e *Engine) Scan() error { return e.scan(nil) }
func (e *Engine) scan(watch func(string)) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	seen := map[string]bool{}
	for _, c := range e.discover(watch) {
		id := digest(c.root.Agent + "\x00" + c.root.Path + "\x00" + c.path)
		seen[id] = true
		src := e.sources[id]
		if src == nil {
			src = &Source{ID: id, Root: c.root, Path: c.path, Health: "healthy", Generation: c.generation}
			if info, err := os.Stat(c.path); err == nil {
				src.Bootstrap = info.Size()
			}
		}
		next := cloneSource(src)
		next.Bootstrap = src.Bootstrap
		if c.ambiguous {
			next.Health, next.Reason = "degraded", "ambiguous_encoding"
			if err := e.commit(next, nil); err != nil {
				return err
			}
			continue
		}
		events := e.readSource(next)
		if next.Session != nil {
			s := next.Session
			if s.State != "idle" && (s.UpdatedAt.IsZero() || time.Since(s.UpdatedAt) > e.Stale) {
				s.State, s.Confidence, s.Reason = "unknown", "unknown", "stale_evidence"
			}
		}
		// State changes are published after reducing a complete read batch, so a
		// fast asked/decided pair never flashes a stale approval overlay.
		before, _ := json.Marshal(src.Session)
		after := publicSession(next, time.Now(), e.Stale)
		afterBytes, _ := json.Marshal(next.Session)
		if !bytes.Equal(before, afterBytes) || src.Health != next.Health {
			if after != nil {
				events = append(events, Event{Type: "state.changed", Session: after})
			}
		}
		// Avoid rewriting checkpoints for every unchanged historical file.
		oldJSON, _ := json.Marshal(src)
		newJSON, _ := json.Marshal(next)
		if !bytes.Equal(oldJSON, newJSON) || e.sources[id] == nil || len(events) > 0 {
			if err := e.commit(next, events); err != nil {
				return err
			}
		}
	}
	for id, src := range e.sources {
		if seen[id] || src.Health == "unavailable" || src.Health == "superseded" {
			continue
		}
		next := cloneSource(src)
		next.Bootstrap = src.Bootstrap
		next.Health, next.Reason = "unavailable", "source_missing_or_root_unavailable"
		if src.Root.Agent == "deepseek" {
			for _, other := range e.sources {
				if seen[other.ID] && filepath.Dir(other.Path) == filepath.Dir(src.Path) && other.Generation > src.Generation {
					next.Health, next.Reason = "superseded", "new_generation"
				}
			}
		}
		events := []Event{}
		if next.Session != nil {
			events = append(events, Event{Type: "source.degraded", Session: publicSession(next, time.Now(), e.Stale)})
		}
		if err := e.commit(next, events); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) commit(src *Source, events []Event) error {
	if err := e.Store.Commit(src, events); err != nil {
		return err
	}
	e.sources[src.ID] = src
	return nil
}

func (e *Engine) readSource(src *Source) []Event {
	events := []Event{}
	f, err := os.Open(src.Path)
	if err != nil {
		src.Health, src.Reason = "unavailable", "source_open_failed"
		return events
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		src.Health, src.Reason = "unavailable", "source_stat_failed"
		return events
	}
	prefix, anchor := anchors(f, src.Offset)
	if src.ParserRevision != 1 || info.Size() < src.Offset || prefix != src.Prefix || anchor != src.Anchor {
		// A rebuilt source is a new historical baseline, not a new task.
		src.Offset, src.Prefix, src.Anchor, src.Session, src.Parser, src.Bootstrap = 0, "", "", nil, ParserState{}, info.Size()
		src.ParserRevision = 1
	}
	src.Health, src.Reason, src.ModTime = "healthy", "", info.ModTime().UTC()
	if src.Root.Agent == "deepseek" && src.Offset == 0 {
		if err := prepareSeed(src, f, info.Size()); err != nil {
			src.Health, src.Reason = "degraded", err.Error()
			return events
		}
	}
	units, readErr := readUnits(f, src.Offset, info.Size())
	for _, u := range units {
		trial := cloneSource(src)
		trial.Bootstrap = src.Bootstrap
		unitEvents := []Event{}
		for _, line := range u.Lines {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			facts, err := parse(trial, line)
			if err != nil {
				src.Health, src.Reason = "degraded", err.Error()
				if trial.Parser.Format > 0 {
					src.Parser.Format = trial.Parser.Format
				}
				src.Prefix, src.Anchor = anchors(f, src.Offset)
				return events
			}
			for _, fact := range facts {
				if fact.At.IsZero() {
					fact.At = src.ModTime
				}
				ended := apply(trial.Session, fact)
				if ended && !fact.Silent && u.End > src.Bootstrap && trial.Session != nil {
					s := *trial.Session
					s.SourceHealth, s.Freshness = "healthy", "fresh"
					unitEvents = append(unitEvents, Event{Type: "turn.ended", Session: &s})
				}
			}
		}
		trial.Offset = u.End
		*src = *trial
		events = append(events, unitEvents...)
	}
	if readErr != nil {
		src.Health, src.Reason = "degraded", readErr.Error()
	}
	src.Prefix, src.Anchor = anchors(f, src.Offset)
	return events
}

func (e *Engine) Snapshot() (Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, revision, err := e.Store.Events(1 << 62)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{Schema: 1, Epoch: e.Store.Epoch, Revision: revision, Sessions: []*Session{}}
	byID := map[string]*Session{}
	for _, src := range e.sources {
		if src.Health == "superseded" {
			continue
		}
		allowed := false
		for _, r := range e.Roots {
			if src.Root == r {
				allowed = true
				break
			}
		}
		if !allowed {
			continue
		}
		if s := publicSession(src, time.Now(), e.Stale); s != nil {
			if prior := byID[s.ID]; prior == nil || s.UpdatedAt.After(prior.UpdatedAt) {
				byID[s.ID] = s
			}
		}
	}
	for _, s := range byID {
		snapshot.Sessions = append(snapshot.Sessions, s)
	}
	sortSessions(snapshot.Sessions)
	return snapshot, nil
}

func (e *Engine) Diagnostics() any {
	e.mu.Lock()
	defer e.mu.Unlock()
	sources := []SourceStatus{}
	for _, src := range e.sources {
		sources = append(sources, SourceStatus{ID: src.ID, Agent: src.Root.Agent, Health: src.Health, Reason: src.Reason, Format: src.Parser.Format})
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	roots := map[string]string{}
	for k, v := range e.RootHealth {
		roots[k] = v
	}
	return struct {
		Roots   map[string]string `json:"roots"`
		Sources []SourceStatus    `json:"sources"`
		Watcher string            `json:"watcher"`
	}{roots, sources, e.WatcherHealth}
}

// Filesystem notifications wake a bounded, coalesced scan. Periodic scans also
// discover roots/directories created after startup and recover lost notifications.
func (e *Engine) Run(ctx context.Context, interval time.Duration) error {
	watcher, err := fsnotify.NewWatcher()
	var watch func(string)
	var signals <-chan struct{}
	if err == nil {
		defer watcher.Close()
		known := map[string]bool{}
		watch = func(path string) {
			if !known[path] {
				if watcher.Add(path) == nil {
					known[path] = true
				}
			}
		}
		wake := make(chan struct{}, 1)
		signals = wake
		go func() {
			for {
				select {
				case _, ok := <-watcher.Events:
					if !ok {
						return
					}
					select {
					case wake <- struct{}{}:
					default:
					}
				case _, ok := <-watcher.Errors:
					if !ok {
						return
					}
					e.mu.Lock()
					e.WatcherHealth = "degraded_polling_fallback"
					e.mu.Unlock()
				case <-ctx.Done():
					return
				}
			}
		}()
		e.mu.Lock()
		e.WatcherHealth = "fsnotify_with_polling"
		e.mu.Unlock()
	}
	if err := e.scan(watch); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	debounce := time.NewTicker(250 * time.Millisecond)
	defer debounce.Stop()
	dirty := false
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-signals:
			dirty = true
		case <-ticker.C:
			if err := e.scan(watch); err != nil {
				return err
			}
			dirty = false
		case <-debounce.C:
			if dirty {
				if err := e.scan(watch); err != nil {
					return err
				}
				dirty = false
			}
		}
	}
}
