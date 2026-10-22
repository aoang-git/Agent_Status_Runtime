package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	sources       map[string]*Source
	RootHealth    map[string]string
	WatcherHealth string
	bootstrapped  bool
	startedAt     time.Time
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

func NewEngine(store *Store, roots []Root) (*Engine, error) {
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
	return &Engine{Store: store, Roots: roots, sources: sources, RootHealth: map[string]string{}, WatcherHealth: "polling", startedAt: time.Now().UTC()}, nil
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
			if info, err := os.Stat(c.path); err == nil && !e.bootstrapped {
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
		if next.Session != nil && (src.Health != next.Health || src.Reason != next.Reason) {
			events = append(events, healthFact(next))
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
		events := []Fact{}
		if next.Session != nil {
			events = append(events, healthFact(next))
		}
		if err := e.commit(next, events); err != nil {
			return err
		}
	}
	e.bootstrapped = true
	return nil
}

func (e *Engine) commit(src *Source, events []Fact) error {
	if err := e.Store.Commit(src, events); err != nil {
		return err
	}
	e.sources[src.ID] = src
	return nil
}

func (e *Engine) readSource(src *Source) []Fact {
	events := []Fact{}
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
	if src.Incarnation == "" {
		src.Incarnation = randomToken()
		src.ParserRevision = 2
	} else if src.ParserRevision != 2 || info.Size() < src.Offset || prefix != src.Prefix || anchor != src.Anchor {
		// A rebuilt source is a new historical baseline, not a new task.
		src.Offset, src.Prefix, src.Anchor, src.Session, src.Parser, src.Bootstrap = 0, "", "", nil, ParserState{}, info.Size()
		src.ParserRevision = 2
		src.Incarnation = randomToken()
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
		unitEvents := []Fact{}
		for lineIndex, line := range u.Lines {
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
			for factIndex, fact := range facts {
				if trial.Session == nil {
					continue
				}
				fact.Schema = 2
				fact.ProducerID = "file:" + trial.ID + ":" + trial.Incarnation
				fact.EventID = digest(fmt.Sprintf("%d:%d:%d", u.End, lineIndex, factIndex))
				fact.Agent, fact.Namespace, fact.SessionID = trial.Root.Agent, trial.Session.Namespace, trial.Session.ID
				fact.SourceID, fact.Origin = trial.ID, "file"
				fact.Historical = fact.Historical || u.End <= src.Bootstrap || fact.OccurredAt != nil && fact.OccurredAt.Before(e.startedAt)
				encoded, _ := json.Marshal(fact.InputEvent)
				if _, err := DecodeInput(encoded); err != nil {
					src.Health, src.Reason = "degraded", "invalid_native_metadata"
					src.Prefix, src.Anchor = anchors(f, src.Offset)
					return events
				}
				if fact.Kind == "tool.observed" && fact.Data.CallID == "" {
					src.Health, src.Reason = "degraded", "missing_tool_id"
					src.Prefix, src.Anchor = anchors(f, src.Offset)
					return events
				}
				unitEvents = append(unitEvents, fact)
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

func (e *Engine) Snapshot() (Snapshot, error) { return e.Store.Snapshot() }
func healthFact(src *Source) Fact {
	f := observation("source.health", "source_health", "", time.Now().UTC(), EventData{Health: src.Health, Reason: src.Reason})
	f.ProducerID = "file:" + src.ID + ":" + src.Incarnation
	f.EventID = randomToken()
	f.Agent, f.Namespace, f.SessionID = src.Root.Agent, src.Session.Namespace, src.Session.ID
	f.SourceID, f.Origin = src.ID, "file"
	return f
}

func (e *Engine) Diagnostics() any {
	e.mu.Lock()
	defer e.mu.Unlock()
	sources := []SourceStatus{}
	for _, src := range e.sources {
		sources = append(sources, SourceStatus{ID: src.ID, Agent: src.Root.Agent, Namespace: namespace(src.Root), Health: src.Health, Reason: src.Reason, Format: src.Parser.Format, Origin: "file"})
	}
	if snapshot, err := e.Store.Snapshot(); err == nil {
		seen := map[string]bool{}
		for _, ledger := range snapshot.Sessions {
			if ledger.Origin != "external" {
				continue
			}
			key := ledger.SourceID + "\x00" + ledger.Agent + "\x00" + ledger.Namespace
			if seen[key] {
				continue
			}
			seen[key] = true
			reason := "external_report_only"
			if ledger.Coverage != "complete" {
				reason = "sequence_gap_or_retention"
			}
			sources = append(sources, SourceStatus{ID: ledger.SourceID, Agent: ledger.Agent, Namespace: ledger.Namespace, Origin: "external", Health: "reported", Reason: reason})
		}
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
