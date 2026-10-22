package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

const Version = "0.1.0"

type Root struct {
	Agent string `json:"agent"`
	Path  string `json:"path"`
}

// Session contains only status metadata. Conversation text and tool arguments
// never leave the parser.
type Session struct {
	ID           string          `json:"session_id"`
	NativeID     string          `json:"native_session_id"`
	Agent        string          `json:"agent"`
	Surface      string          `json:"surface"`
	AgentVersion string          `json:"agent_version,omitempty"`
	ParentID     string          `json:"parent_session_id,omitempty"`
	TurnID       string          `json:"turn_id,omitempty"`
	State        string          `json:"state"`
	Confidence   string          `json:"confidence"`
	Reason       string          `json:"reason"`
	Outcome      string          `json:"outcome,omitempty"`
	UpdatedAt    time.Time       `json:"updated_at"`
	SourceHealth string          `json:"source_health"`
	Freshness    string          `json:"freshness"`
	Tools        map[string]bool `json:"pending_tools,omitempty"`
	Approvals    map[string]bool `json:"pending_approvals,omitempty"`
}

type ParserState struct {
	Format    int    `json:"format,omitempty"`
	LastNode  string `json:"last_node,omitempty"`
	Seeded    bool   `json:"seeded,omitempty"`
	SeedReady bool   `json:"seed_ready,omitempty"`
	SeedCut   int64  `json:"seed_cut,omitempty"`
	NextSeq   int64  `json:"next_seq,omitempty"`
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
	ParserRevision int         `json:"parser_revision"`
	Bootstrap      int64       `json:"-"`
}

type Fact struct {
	Kind, Turn, Call, Outcome, Reason string
	At                                time.Time
	Silent                            bool
}

type Event struct {
	ID      int64    `json:"id"`
	Type    string   `json:"type"`
	Session *Session `json:"session,omitempty"`
}

type Snapshot struct {
	Schema   int        `json:"schema"`
	Epoch    string     `json:"epoch"`
	Revision int64      `json:"revision"`
	Sessions []*Session `json:"sessions"`
}

type SourceStatus struct {
	ID     string `json:"id"`
	Agent  string `json:"agent"`
	Health string `json:"health"`
	Reason string `json:"reason,omitempty"`
	Format int    `json:"format,omitempty"`
}

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:16])
}

func newSession(src *Source, native, surface, version, parent string) {
	if native == "" {
		return
	}
	if src.Session == nil || src.Session.NativeID != native {
		src.Session = &Session{ID: digest(src.Root.Agent + "\x00" + src.Root.Path + "\x00" + native), NativeID: native,
			Agent: src.Root.Agent, State: "unknown", Confidence: "unknown", Reason: "no_activity_evidence",
			Tools: map[string]bool{}, Approvals: map[string]bool{}}
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
	var copy Source
	_ = json.Unmarshal(b, &copy)
	return &copy
}

func apply(s *Session, f Fact) bool {
	if s == nil || f.Kind == "" {
		return false
	}
	if !f.At.IsZero() && f.At.Before(s.UpdatedAt) {
		return false
	}
	if f.Kind != "start" && f.Turn != "" && s.TurnID != "" && f.Turn != s.TurnID {
		return false
	}
	if s.Tools == nil {
		s.Tools = map[string]bool{}
	}
	if s.Approvals == nil {
		s.Approvals = map[string]bool{}
	}
	if f.Kind == "start" {
		if f.Turn == "" {
			f.Turn = "local:" + f.At.Format(time.RFC3339Nano)
		}
		if s.TurnID == f.Turn && s.Outcome != "" {
			return false
		}
		if s.TurnID != f.Turn {
			s.Tools, s.Approvals = map[string]bool{}, map[string]bool{}
			s.Outcome = ""
		}
		s.TurnID = f.Turn
	} else if s.Outcome != "" {
		// A late tool/result must not reopen a terminated turn.
		return false
	}
	if !f.At.IsZero() {
		s.UpdatedAt = f.At
	}
	s.Confidence, s.Reason = "observed", f.Reason
	switch f.Kind {
	case "start", "activity":
		s.State = "working"
	case "tool":
		s.Tools[f.Call] = true
		s.State, s.Confidence = "tool_activity", "inferred"
	case "tool_end":
		delete(s.Tools, f.Call)
		s.State = "working"
	case "approval":
		s.Approvals[f.Call] = true
		s.State, s.Confidence = "waiting_confirmation", "inferred"
	case "approval_end":
		delete(s.Approvals, f.Call)
		s.State = "working"
	case "candidate", "branch":
		s.State, s.Confidence = "unknown", "inferred"
		if f.Kind == "branch" {
			s.Tools, s.Approvals = map[string]bool{}, map[string]bool{}
			s.TurnID, s.Outcome = "", ""
		}
	case "end":
		s.Outcome, s.State = f.Outcome, "idle"
		s.Tools, s.Approvals = map[string]bool{}, map[string]bool{}
		if f.Outcome == "unknown" {
			s.State, s.Confidence = "unknown", "unknown"
		}
		return true
	}
	if f.Kind != "candidate" && f.Kind != "branch" {
		if len(s.Approvals) > 0 {
			s.State, s.Confidence = "waiting_confirmation", "inferred"
		} else if len(s.Tools) > 0 {
			s.State, s.Confidence = "tool_activity", "inferred"
		}
	}
	return false
}

func publicSession(src *Source, now time.Time, stale time.Duration) *Session {
	if src.Session == nil {
		return nil
	}
	s := *src.Session
	s.SourceHealth, s.Freshness = src.Health, "fresh"
	if s.UpdatedAt.IsZero() || now.Sub(s.UpdatedAt) > stale {
		s.Freshness = "stale"
		if s.State != "idle" {
			s.State, s.Confidence, s.Reason = "unknown", "unknown", "stale_evidence"
		}
	}
	if src.Health != "healthy" {
		s.State, s.Confidence, s.Reason = "unknown", "unknown", src.Reason
	}
	return &s
}

func sortSessions(s []*Session) {
	sort.Slice(s, func(i, j int) bool { return s[i].ID < s[j].ID })
}
