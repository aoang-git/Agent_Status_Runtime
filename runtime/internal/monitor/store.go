package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const MaxRecentBytes = 8 << 20
const DedupLimit = 10000
const StreamLimit = 4096
const Retention = 10 * time.Minute

var ErrConflict = errors.New("event_id_conflict")

type receipt struct {
	ID   int64
	Hash string
	At   time.Time
}
type sequence struct {
	Highest int64
	At      time.Time
}
type Recent struct {
	Schema   int    `json:"schema"`
	Epoch    string `json:"epoch"`
	Revision int64  `json:"revision"`
	Facts    []Fact `json:"facts"`
	Dropped  uint64 `json:"dropped"`
}

// Store is bounded and process-local; changed wakes every subscriber.
type Store struct {
	mu        sync.Mutex
	Epoch     string
	facts     []Fact
	sizes     []int
	bytes     int
	revision  int64
	dropped   uint64
	dedup     map[string]receipt
	sequences map[string]sequence
	changed   chan struct{}
	now       func() time.Time
}

func NewStore() *Store {
	return &Store{Epoch: randomToken(), dedup: map[string]receipt{}, sequences: map[string]sequence{}, changed: make(chan struct{}), now: time.Now}
}
func (s *Store) prune(now time.Time) {
	n := 0
	for n < len(s.facts) && (len(s.facts)-n > EventLimit || s.bytes > MaxRecentBytes || !s.facts[n].ReceivedAt.After(now.Add(-Retention))) {
		s.bytes -= s.sizes[n]
		s.facts[n] = Fact{}
		n++
		s.dropped++
	}
	s.facts, s.sizes = s.facts[n:], s.sizes[n:]
	for k, v := range s.dedup {
		if !v.At.After(now.Add(-Retention)) {
			delete(s.dedup, k)
		}
	}
	for k, v := range s.sequences {
		if !v.At.After(now.Add(-time.Hour)) {
			delete(s.sequences, k)
		}
	}
}
func (s *Store) append(f Fact, now time.Time) int64 {
	s.revision++
	f.ID = s.revision
	f.ReceivedAt = now.UTC()
	b, _ := json.Marshal(f)
	var copied Fact
	_ = json.Unmarshal(b, &copied)
	f = copied
	s.facts = append(s.facts, f)
	s.sizes = append(s.sizes, len(b))
	s.bytes += len(b)
	s.prune(now)
	close(s.changed)
	s.changed = make(chan struct{})
	return f.ID
}
func (s *Store) Commit(_ *Source, facts []Fact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range facts {
		if !f.Historical {
			s.append(f, s.now())
		}
	}
	return nil
}
func (s *Store) Ingest(ctx context.Context, f InputEvent) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.prune(now)
	b, _ := json.Marshal(f)
	hash := digest(string(b))
	key := f.ProducerID + "\x00" + f.EventID
	if v, ok := s.dedup[key]; ok {
		if v.Hash != hash {
			return 0, false, ErrConflict
		}
		return v.ID, true, nil
	}
	fact := Fact{InputEvent: f, Origin: "external", SourceID: "external:" + digest(f.ProducerID)}
	if f.SourceSequence != nil {
		key := f.ProducerID + "\x00" + f.StreamID
		highest := *f.SourceSequence
		if v, ok := s.sequences[key]; ok {
			fact.SequenceGap = *f.SourceSequence != v.Highest+1
			if v.Highest > highest {
				highest = v.Highest
			}
		}
		if _, exists := s.sequences[key]; !exists && len(s.sequences) >= StreamLimit {
			oldest := ""
			var at time.Time
			for k, v := range s.sequences {
				if oldest == "" || v.At.Before(at) {
					oldest, at = k, v.At
				}
			}
			delete(s.sequences, oldest)
		}
		s.sequences[key] = sequence{highest, now}
	}
	id := s.append(fact, now)
	if len(s.dedup) >= DedupLimit {
		oldest := ""
		var at time.Time
		for k, v := range s.dedup {
			if oldest == "" || v.At.Before(at) {
				oldest, at = k, v.At
			}
		}
		delete(s.dedup, oldest)
	}
	s.dedup[key] = receipt{id, hash, now}
	return id, false, nil
}
func (s *Store) Recent() Recent {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(s.now())
	return Recent{3, s.Epoch, s.revision, append([]Fact{}, s.facts...), s.dropped}
}

// Read atomically captures events, the boundary and the notification channel.
func (s *Store) Read(after int64) ([]Fact, int64, bool, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(s.now())
	first := s.revision + 1
	if len(s.facts) > 0 {
		first = s.facts[0].ID
	}
	gap := after > s.revision || after+1 < first
	facts := []Fact{}
	if !gap {
		for _, f := range s.facts {
			if f.ID > after {
				facts = append(facts, f)
			}
		}
	}
	return facts, s.revision, gap, s.changed
}
func (s *Store) Stats() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(s.now())
	return map[string]any{"events": len(s.facts), "bytes": s.bytes, "dropped": s.dropped, "dedup": len(s.dedup), "streams": len(s.sequences)}
}
