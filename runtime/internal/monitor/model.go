package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

const Version = "0.2.0"
const EventLimit = 4096
const MaxInput = 16 << 10
const MaxSnapshotBytes = 8 << 20

type Root struct {
	Agent string `json:"agent"`
	Path  string `json:"path"`
}

// Session contains identity only; consumers interpret activity and expiry.
type Session struct {
	ID           string `json:"session_id"`
	Namespace    string `json:"session_namespace"`
	Agent        string `json:"agent"`
	Surface      string `json:"surface"`
	AgentVersion string `json:"agent_version,omitempty"`
	ParentID     string `json:"parent_session_id,omitempty"`
}

type EventData struct {
	Surface         string `json:"surface,omitempty"`
	AgentVersion    string `json:"agent_version,omitempty"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	Role            string `json:"role,omitempty"`
	StopReason      string `json:"stop_reason,omitempty"`
	Phase           string `json:"phase,omitempty"`
	Scope           string `json:"scope,omitempty"`
	Outcome         string `json:"outcome,omitempty"`
	CallID          string `json:"call_id,omitempty"`
	RequestID       string `json:"request_id,omitempty"`
	Decision        string `json:"decision,omitempty"`
	State           string `json:"state,omitempty"`
	MessageID       string `json:"message_id,omitempty"`
	ParentNodeID    string `json:"parent_node_id,omitempty"`
	BranchChanged   *bool  `json:"branch_changed,omitempty"`
	HasTools        *bool  `json:"has_tools,omitempty"`
	Health          string `json:"health,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

type InputEvent struct {
	Schema         int        `json:"schema"`
	ProducerID     string     `json:"producer_id"`
	EventID        string     `json:"event_id"`
	Agent          string     `json:"agent"`
	Namespace      string     `json:"session_namespace"`
	SessionID      string     `json:"session_id"`
	OperationID    string     `json:"operation_id,omitempty"`
	OccurredAt     *time.Time `json:"occurred_at,omitempty"`
	Kind           string     `json:"kind"`
	Data           EventData  `json:"data"`
	NativeType     string     `json:"native_type,omitempty"`
	NativeReason   string     `json:"native_reason,omitempty"`
	ReportedBasis  string     `json:"reported_basis,omitempty"`
	StreamID       string     `json:"stream_id,omitempty"`
	SourceSequence *int64     `json:"source_sequence,omitempty"`
}

type Fact struct {
	InputEvent
	ID          int64     `json:"id"`
	ReceivedAt  time.Time `json:"received_at"`
	Origin      string    `json:"origin"`
	Historical  bool      `json:"historical"`
	SourceID    string    `json:"source_id"`
	SequenceGap bool      `json:"sequence_gap,omitempty"`
}

type ParserState struct {
	Format    int               `json:"format,omitempty"`
	LastNode  string            `json:"last_node,omitempty"`
	Operation string            `json:"operation,omitempty"`
	Calls     map[string]string `json:"calls,omitempty"`
	Seeded    bool              `json:"seeded,omitempty"`
	SeedReady bool              `json:"seed_ready,omitempty"`
	SeedCut   int64             `json:"seed_cut,omitempty"`
	NextSeq   int64             `json:"next_seq,omitempty"`
}

type Source struct {
	ID             string      `json:"id"`
	Root           Root        `json:"root"`
	Path           string      `json:"path"`
	Offset         int64       `json:"offset"`
	Prefix         string      `json:"prefix,omitempty"`
	Anchor         string      `json:"anchor,omitempty"`
	Health         string      `json:"health"`
	Reason         string      `json:"reason,omitempty"`
	Parser         ParserState `json:"parser"`
	Session        *Session    `json:"session,omitempty"`
	ModTime        time.Time   `json:"mod_time"`
	Generation     int         `json:"generation,omitempty"`
	Incarnation    string      `json:"incarnation"`
	ParserRevision int         `json:"parser_revision"`
	Bootstrap      int64       `json:"-"`
}

type EvidenceSession struct {
	Agent                string   `json:"agent"`
	Namespace            string   `json:"session_namespace"`
	SessionID            string   `json:"session_id"`
	SourceID             string   `json:"source_id"`
	Origin               string   `json:"origin"`
	Facts                []Fact   `json:"facts"`
	Coverage             string   `json:"coverage"`
	IncompleteOperations []string `json:"incomplete_operations"`
}

type Snapshot struct {
	Schema   int               `json:"schema"`
	Epoch    string            `json:"epoch"`
	Revision int64             `json:"revision"`
	Sessions []EvidenceSession `json:"sessions"`
	Coverage string            `json:"coverage"`
}

type SourceStatus struct {
	ID        string `json:"id"`
	Agent     string `json:"agent"`
	Namespace string `json:"session_namespace"`
	Health    string `json:"health"`
	Reason    string `json:"reason,omitempty"`
	Format    int    `json:"format,omitempty"`
	Origin    string `json:"origin"`
}

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:16])
}
func namespace(root Root) string { return digest(root.Agent + "\x00" + root.Path) }
func newSession(src *Source, id, surface, version, parent string) {
	if id == "" {
		return
	}
	if src.Session == nil || src.Session.ID != id {
		src.Session = &Session{ID: id, Namespace: namespace(src.Root), Agent: src.Root.Agent, Surface: surface}
	}
	if surface != "unknown" || src.Session.Surface == "" {
		src.Session.Surface = surface
	}
	if version != "" {
		src.Session.AgentVersion = version
	}
	if parent != "" {
		src.Session.ParentID = parent
	}
}
func cloneSource(src *Source) *Source {
	b, _ := json.Marshal(src)
	var next Source
	_ = json.Unmarshal(b, &next)
	next.Bootstrap = src.Bootstrap
	return &next
}

// This ledger retains observations, not business states. Pair keys never cross
// sources or operations. Source timestamps do not reorder accepted facts.
func retainEvidence(s *EvidenceSession, f Fact) {
	s.Facts = append(s.Facts, f)
	if f.SequenceGap {
		s.Coverage = "partial"
		markIncomplete(s, f.OperationID)
	}
	ops := []string{}
	seen := map[string]bool{}
	for i := len(s.Facts) - 1; i >= 0; i-- {
		if s.Facts[i].Kind == "source.heartbeat" || s.Facts[i].Kind == "source.health" || s.Facts[i].Kind == "session.observed" {
			continue
		}
		op := s.Facts[i].OperationID
		if op != "" && !seen[op] {
			seen[op] = true
			ops = append(ops, op)
		}
	}
	allowed := map[string]bool{"": true}
	for i, op := range ops {
		if i < 32 {
			allowed[op] = true
		} else {
			s.Coverage = "partial"
		}
	}
	latest := map[string]Fact{}
	for _, v := range s.Facts {
		if !allowed[v.OperationID] {
			continue
		}
		key := v.OperationID + "\x00" + v.Kind + "\x00" + v.Data.Scope
		switch v.Kind {
		case "tool.observed":
			pair := key + "\x00pair\x00" + v.Data.CallID
			if v.Data.Phase == "finished" {
				delete(latest, pair)
			} else {
				latest[pair] = v
			}
			key += "\x00last"
		case "approval.observed":
			pair := key + "\x00pair\x00" + v.Data.RequestID
			if v.Data.Phase == "resolved" {
				delete(latest, pair)
			} else {
				latest[pair] = v
			}
			key += "\x00last"
		case "lifecycle.observed":
			key += "\x00" + v.Data.Phase
		case "source.heartbeat":
			key = "source.heartbeat"
		}
		latest[key] = v
	}
	s.Facts = s.Facts[:0]
	seenIDs := map[int64]bool{}
	for _, v := range latest {
		if !seenIDs[v.ID] {
			s.Facts = append(s.Facts, v)
			seenIDs[v.ID] = true
		}
	}
	sort.Slice(s.Facts, func(i, j int) bool { return s.Facts[i].ID < s.Facts[j].ID })
	if len(s.Facts) > 2048 {
		for _, lost := range s.Facts[:len(s.Facts)-2048] {
			markIncomplete(s, lost.OperationID)
		}
		s.Facts = s.Facts[len(s.Facts)-2048:]
		s.Coverage = "partial"
	}
}

func markIncomplete(s *EvidenceSession, op string) {
	if op == "" {
		op = "*"
	}
	for _, prior := range s.IncompleteOperations {
		if prior == op {
			return
		}
	}
	s.IncompleteOperations = append(s.IncompleteOperations, op)
	if len(s.IncompleteOperations) > 33 {
		s.IncompleteOperations = []string{"*"}
	}
}
