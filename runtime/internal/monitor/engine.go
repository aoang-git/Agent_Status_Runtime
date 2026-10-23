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
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

type Engine struct {
	mu            sync.Mutex
	scanMu        sync.Mutex
	ctx           context.Context
	progress      atomic.Int64
	health        Health
	capacity      bool
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
	var err error
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
	e := &Engine{Store: store, Roots: roots, sources: map[string]*Source{}, RootHealth: map[string]string{}, WatcherHealth: "polling", startedAt: time.Now().UTC(), ctx: context.Background()}
	e.progress.Store(time.Now().UnixNano())
	e.publish(time.Time{}, 0)
	return e, nil
}

type candidate struct {
	root       Root
	path       string
	generation int
	ambiguous  bool
	modified   time.Time
}

var generationPattern = regexp.MustCompile(`^session(?:\.v([1-9][0-9]*))?\.jsonl(?:\.zstd)?$`)

func (e *Engine) discover(watch func(string)) []candidate {
	all := []candidate{}
	for _, root := range e.Roots {
		health := "healthy"
		if _, err := os.Stat(root.Path); errors.Is(err, os.ErrNotExist) {
			health = "missing"
		}
		byDir := map[string][]candidate{}
		err := filepath.WalkDir(root.Path, func(path string, d fs.DirEntry, err error) error {
			if e.ctx.Err() != nil {
				return e.ctx.Err()
			}
			e.progress.Store(time.Now().UnixNano())
			if err != nil {
				if health != "missing" {
					health = "unavailable"
					if errors.Is(err, os.ErrPermission) {
						health = "permission_denied"
					}
				}
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
				info, _ := d.Info()
				var modified time.Time
				if info != nil {
					modified = info.ModTime()
				}
				c := candidate{root: root, path: path, generation: n, modified: modified}
				dir := filepath.Dir(path)
				byDir[dir] = append(byDir[dir], c)
				if len(byDir) > 8192 {
					dirs := make([]candidate, 0, len(byDir))
					for _, list := range byDir {
						dirs = append(dirs, list[0])
					}
					dirs = limitCandidates(dirs)
					keep := map[string]bool{}
					for _, item := range dirs {
						keep[filepath.Dir(item.path)] = true
					}
					for key := range byDir {
						if !keep[key] {
							delete(byDir, key)
						}
					}
					e.capacity = true
				}
			} else if strings.HasSuffix(d.Name(), ".jsonl") {
				info, _ := d.Info()
				var modified time.Time
				if info != nil {
					modified = info.ModTime()
				}
				all = append(all, candidate{root: root, path: path, modified: modified})
				if len(all) > 8192 {
					all = limitCandidates(all)
					e.capacity = true
				}
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
	if len(all) > 4096 {
		all = limitCandidates(all)
		e.capacity = true
	}
	sort.Slice(all, func(i, j int) bool { return all[i].path < all[j].path })
	return all
}

func limitCandidates(all []candidate) []candidate {
	sort.Slice(all, func(i, j int) bool {
		if all[i].modified.Equal(all[j].modified) {
			return all[i].path < all[j].path
		}
		return all[i].modified.After(all[j].modified)
	})
	return all[:4096]
}
func (e *Engine) Scan() error { return e.scan(nil) }
func (e *Engine) scan(watch func(string)) error {
	e.scanMu.Lock()
	defer e.scanMu.Unlock()
	start := time.Now()
	seen := map[string]bool{}
	e.capacity = false
	candidates := e.discover(watch)
	for _, c := range candidates {
		seen[digest(c.root.Agent+"\x00"+c.root.Path+"\x00"+c.path)] = true
	}
	projected := len(e.sources) + len(candidates)
	for id := range e.sources {
		if seen[id] {
			projected--
		}
	}
	for id, src := range e.sources {
		if seen[id] {
			continue
		}
		if projected > 4096 || time.Since(src.LastSeen) > time.Hour {
			delete(e.sources, id)
			projected--
			continue
		}
		if src.Health != "unavailable" && src.Health != "superseded" {
			src.Health, src.Reason = "unavailable", "source_missing_or_root_unavailable"
			if src.Root.Agent == "deepseek" {
				for _, c := range candidates {
					if filepath.Dir(c.path) == filepath.Dir(src.Path) && c.generation > src.Generation {
						src.Health, src.Reason = "superseded", "new_generation"
					}
				}
			}
			if src.Session != nil {
				_ = e.Store.Commit(src, []Fact{healthFact(src)})
			}
		}
	}
	for i, c := range candidates {
		if e.ctx.Err() != nil {
			return e.ctx.Err()
		}
		id := digest(c.root.Agent + "\x00" + c.root.Path + "\x00" + c.path)
		src := e.sources[id]
		if src == nil {
			src = &Source{ID: id, Root: c.root, Path: c.path, Health: "healthy", Generation: c.generation}
			if info, err := os.Stat(c.path); err == nil && (!e.bootstrapped || e.capacity || info.ModTime().Before(e.startedAt)) {
				src.Bootstrap = info.Size()
			}
		}
		if c.ambiguous {
			src.Health, src.Reason = "degraded", "ambiguous_encoding"
			src.LastSeen = time.Now()
			e.sources[id] = src
			continue
		}
		e.process(src)
		if i%64 == 0 {
			e.publish(time.Time{}, 0)
		}
	}
	e.bootstrapped = true
	e.progress.Store(time.Now().UnixNano())
	e.publish(time.Now().UTC(), time.Since(start))
	return e.ctx.Err()
}
func (e *Engine) process(src *Source) {
	next := cloneSource(src)
	if info, err := os.Stat(src.Path); err == nil {
		next.LastSeen = time.Now()
		_ = info
	}
	events := e.readSource(next)
	if next.Session != nil && (src.Health != next.Health || src.Reason != next.Reason) {
		events = append(events, healthFact(next))
	}
	_ = e.Store.Commit(next, events)
	e.sources[next.ID] = next
	e.progress.Store(time.Now().UnixNano())
}
func (e *Engine) poll(paths map[string]bool) error {
	e.scanMu.Lock()
	defer e.scanMu.Unlock()
	start := time.Now()
	for _, src := range e.sources {
		if e.ctx.Err() != nil {
			return e.ctx.Err()
		}
		if paths != nil && !paths[src.Path] {
			continue
		}
		e.process(src)
	}
	e.publish(time.Now().UTC(), time.Since(start))
	e.progress.Store(time.Now().UnixNano())
	return nil
}
func (e *Engine) readSource(src *Source) []Fact {
	events := []Fact{}
	f, err := os.Open(src.Path)
	if err != nil {
		src.Health, src.Reason = "unavailable", "source_open_failed"
		if errors.Is(err, os.ErrPermission) {
			src.Reason = "source_permission_denied"
		}
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
		src.ParserRevision = 3
	} else if src.ParserRevision != 3 || info.Size() < src.Offset || prefix != src.Prefix || anchor != src.Anchor {
		// A rebuilt source is a new historical baseline, not a new task.
		src.Offset, src.Prefix, src.Anchor, src.Session, src.Parser, src.Bootstrap = 0, "", "", nil, ParserState{}, info.Size()
		src.ParserRevision = 3
		src.Incarnation = randomToken()
	}
	src.Health, src.Reason, src.ModTime = "healthy", "", info.ModTime().UTC()
	if src.Root.Agent == "deepseek" && src.Offset == 0 {
		if err := e.prepareSeed(src, f, info.Size()); err != nil {
			src.Health, src.Reason = "degraded", err.Error()
			return events
		}
	}
	units, readErr := readUnits(f, src.Offset, info.Size())
	for _, u := range units {
		if e.ctx.Err() != nil {
			return events
		}
		e.progress.Store(time.Now().UnixNano())
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
				fact.Schema = 3
				fact.ProducerID = "file:" + trial.ID + ":" + trial.Incarnation
				fact.EventID = digest(fmt.Sprintf("%d:%d:%d", u.End, lineIndex, factIndex))
				fact.Agent, fact.Namespace, fact.SessionID = trial.Root.Agent, trial.Session.Namespace, trial.Session.ID
				fact.SourceID, fact.Origin = trial.ID, "file"
				fact.Historical = fact.Historical || u.End <= src.Bootstrap
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
				if !fact.Historical {
					unitEvents = append(unitEvents, fact)
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

func healthFact(src *Source) Fact {
	f := observation("source.health", "source_health", "", time.Now().UTC(), EventData{Health: src.Health, Reason: src.Reason})
	f.ProducerID = "file:" + src.ID + ":" + src.Incarnation
	f.EventID = randomToken()
	f.Agent, f.Namespace, f.SessionID = src.Root.Agent, src.Session.Namespace, src.Session.ID
	f.SourceID, f.Origin = src.ID, "file"
	return f
}

type Health struct {
	Roots            map[string]string `json:"roots"`
	Sources          []SourceStatus    `json:"sources"`
	Watcher          string            `json:"watcher"`
	StartedAt        time.Time         `json:"started_at"`
	LastScanAt       time.Time         `json:"last_scan_at"`
	ScanMilliseconds int64             `json:"scan_milliseconds"`
	CapacityExceeded bool              `json:"capacity_exceeded"`
	Queue            map[string]any    `json:"queue"`
}

func (e *Engine) publish(at time.Time, duration time.Duration) {
	roots := map[string]string{}
	for k, v := range e.RootHealth {
		roots[k] = v
	}
	sources := []SourceStatus{}
	for _, src := range e.sources {
		sources = append(sources, SourceStatus{ID: src.ID, Agent: src.Root.Agent, Namespace: namespace(src.Root), Health: src.Health, Reason: src.Reason, Format: src.Parser.Format, Origin: "file"})
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if at.IsZero() {
		at = e.health.LastScanAt
		duration = time.Duration(e.health.ScanMilliseconds) * time.Millisecond
	}
	mode := e.health.Watcher
	if mode == "" {
		mode = e.WatcherHealth
	}
	e.health = Health{roots, sources, mode, e.startedAt, at, duration.Milliseconds(), e.capacity, nil}
}
func (e *Engine) Diagnostics() any {
	e.mu.Lock()
	h := e.health
	h.Sources = append([]SourceStatus{}, h.Sources...)
	e.mu.Unlock()
	h.Queue = e.Store.Stats()
	// External source identities are derived from the bounded recent queue.
	seen := map[string]bool{}
	for _, f := range e.Store.Recent().Facts {
		key := f.SourceID + "\x00" + f.Agent + "\x00" + f.Namespace
		if f.Origin == "external" && !seen[key] {
			seen[key] = true
			h.Sources = append(h.Sources, SourceStatus{ID: f.SourceID, Agent: f.Agent, Namespace: f.Namespace, Origin: "external", Health: "reported", Reason: "external_report_only"})
		}
	}
	return h
}
func (e *Engine) setWatcher(value string) { e.mu.Lock(); e.health.Watcher = value; e.mu.Unlock() }
func (e *Engine) Run(parent context.Context, interval time.Duration) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	e.ctx = ctx
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		watcher = nil
		e.WatcherHealth = "degraded_polling_fallback"
	} else {
		e.WatcherHealth = "fsnotify_with_polling"
	}
	e.setWatcher(e.WatcherHealth)
	var watcherMu sync.Mutex
	defer func() {
		watcherMu.Lock()
		defer watcherMu.Unlock()
		if watcher != nil {
			_ = watcher.Close()
		}
	}()
	var events <-chan fsnotify.Event
	var failures <-chan error
	if watcher != nil {
		events = watcher.Events
		failures = watcher.Errors
	}
	type job struct {
		discover bool
		paths    map[string]bool
	}
	jobs := make(chan job, 1)
	done := make(chan error, 1)
	var watchFailed atomic.Bool
	watch := func(path string) {
		watcherMu.Lock()
		defer watcherMu.Unlock()
		if watcher == nil {
			watchFailed.Store(true)
			return
		}
		watched := watcher.WatchList()
		for _, existing := range watched {
			if existing == path {
				return
			}
		}
		if len(watched) >= 4096 {
			watchFailed.Store(true)
			return
		}
		if err := watcher.Add(path); err != nil {
			watchFailed.Store(true)
		}
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				done <- ctx.Err()
				return
			case j := <-jobs:
				var err error
				if j.discover {
					watchFailed.Store(false)
					watcherMu.Lock()
					if watcher != nil {
						for _, path := range watcher.WatchList() {
							if _, statErr := os.Stat(path); statErr != nil {
								_ = watcher.Remove(path)
							}
						}
					}
					watcherMu.Unlock()
					err = e.scan(watch)
					if watchFailed.Load() {
						e.setWatcher("degraded_polling_fallback")
					} else {
						e.setWatcher("fsnotify_with_polling")
					}
				} else {
					err = e.poll(j.paths)
				}
				if err != nil {
					done <- err
					return
				}
			}
		}
	}()
	jobs <- job{discover: true}
	poll := time.NewTicker(interval)
	defer poll.Stop()
	discover := time.NewTicker(30 * time.Second)
	defer discover.Stop()
	debounce := time.NewTicker(250 * time.Millisecond)
	defer debounce.Stop()
	watchdog := time.NewTicker(time.Second)
	defer watchdog.Stop()
	pending := map[string]bool{}
	needDiscovery := false
	needPoll := false
	for {
		select {
		case <-parent.Done():
			cancel()
			<-done
			return nil
		case err := <-done:
			if parent.Err() != nil {
				return nil
			}
			return err
		case event, ok := <-events:
			if !ok {
				events = nil
				failures = nil
				e.setWatcher("degraded_polling_fallback")
				continue
			}
			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				watcherMu.Lock()
				if watcher != nil {
					_ = watcher.Remove(event.Name)
				}
				watcherMu.Unlock()
				needDiscovery = true
			}
			if strings.HasSuffix(event.Name, ".jsonl") || strings.HasSuffix(event.Name, ".zstd") {
				if len(pending) < 4096 {
					pending[event.Name] = true
				} else {
					needPoll = true
				}
			}
			if event.Op&fsnotify.Create != 0 {
				needDiscovery = true
			}
		case _, ok := <-failures:
			if !ok {
				failures = nil
			} else {
				e.setWatcher("degraded_polling_fallback")
				needDiscovery = true
			}
		case <-poll.C:
			needPoll = true
		case <-discover.C:
			if events == nil {
				watcherMu.Lock()
				if watcher != nil {
					_ = watcher.Close()
				}
				watcher, err = fsnotify.NewWatcher()
				if err == nil {
					events, failures = watcher.Events, watcher.Errors
				}
				watcherMu.Unlock()
			}
			needDiscovery = true
		case <-watchdog.C:
			if time.Since(time.Unix(0, e.progress.Load())) > 2*time.Minute {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
				}
				return errors.New("scan_stalled")
			}
		case <-debounce.C:
			if needDiscovery {
				select {
				case jobs <- job{discover: true}:
					needDiscovery = false
					needPoll = false
					pending = map[string]bool{}
				default:
				}
			} else if needPoll {
				select {
				case jobs <- job{}:
					needPoll = false
					pending = map[string]bool{}
				default:
				}
			} else if len(pending) > 0 {
				select {
				case jobs <- job{paths: pending}:
					pending = map[string]bool{}
				default:
				}
			}
		}
	}
}
