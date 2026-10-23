package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

const Version = "0.3.0"
const EventLimit = 4096
const MaxInput = 16 << 10

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
	LastSeen       time.Time   `json:"-"`
	Bootstrap      int64       `json:"-"`
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
	next.LastSeen = src.LastSeen
	return &next
}
