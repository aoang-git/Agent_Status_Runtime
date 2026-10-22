package monitor

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

type object map[string]json.RawMessage

func obj(b json.RawMessage) object     { var o object; _ = json.Unmarshal(b, &o); return o }
func str(o object, k string) string    { var s string; _ = json.Unmarshal(o[k], &s); return s }
func number(o object, k string) int    { var n int; _ = json.Unmarshal(o[k], &n); return n }
func boolean(o object, k string) bool  { var b bool; _ = json.Unmarshal(o[k], &b); return b }
func array(b json.RawMessage) []object { var a []object; _ = json.Unmarshal(b, &a); return a }
func stamp(raw json.RawMessage) time.Time {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		t, _ := time.Parse(time.RFC3339Nano, s)
		return t
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		return time.UnixMilli(n).UTC()
	}
	return time.Time{}
}
func observation(kind, native, op string, t time.Time, data EventData) Fact {
	f := Fact{InputEvent: InputEvent{Schema: 2, Kind: kind, OperationID: op, NativeType: native, Data: data}}
	if !t.IsZero() {
		f.OccurredAt = &t
	}
	return f
}

// Only metadata leaves this parser. Native message endings remain messages;
// neither inactivity nor a message ending becomes an Agent state here.
func parse(src *Source, line []byte) ([]Fact, error) {
	if !json.Valid(line) {
		return nil, errors.New("invalid_json")
	}
	o := obj(line)
	if o == nil || str(o, "type") == "" {
		return nil, errors.New("invalid_record_shape")
	}
	before, _ := json.Marshal(src.Session)
	var facts []Fact
	var err error
	switch src.Root.Agent {
	case "codex":
		facts, err = parseCodex(src, o)
	case "claude":
		facts, err = parseClaude(src, o)
	case "pi":
		facts, err = parsePi(src, o)
	case "deepseek":
		facts, err = parseDeepseek(src, o)
	default:
		err = errors.New("unsupported_agent")
	}
	if err != nil {
		return nil, err
	}
	after, _ := json.Marshal(src.Session)
	if src.Session != nil && string(before) != string(after) {
		t := stamp(o["timestamp"])
		if t.IsZero() {
			t = stamp(o["time"])
		}
		s := src.Session
		f := observation("session.observed", str(o, "type"), "", t, EventData{Surface: s.Surface, AgentVersion: s.AgentVersion, ParentSessionID: s.ParentID})
		facts = append([]Fact{f}, facts...)
	}
	for i := range facts {
		f := &facts[i]
		if f.Kind == "tool.observed" {
			if src.Parser.Calls == nil {
				src.Parser.Calls = map[string]string{}
			}
			if f.Data.Phase == "finished" {
				if op, ok := src.Parser.Calls[f.Data.CallID]; ok {
					f.OperationID = op
					delete(src.Parser.Calls, f.Data.CallID)
				}
			} else {
				src.Parser.Calls[f.Data.CallID] = f.OperationID
			}
		}
	}
	return facts, nil
}

func parseCodex(src *Source, o object) ([]Fact, error) {
	p := obj(o["payload"])
	t := stamp(o["timestamp"])
	k := str(o, "type")
	if k == "session_meta" {
		id := str(p, "id")
		if id == "" {
			id = str(p, "session_id")
		}
		surface := "unknown"
		if strings.Contains(strings.ToLower(str(p, "originator")), "desktop") {
			surface = "desktop"
		} else if contains("cli exec", str(p, "source")) {
			surface = "cli"
		}
		parent := str(p, "parent_thread_id")
		if parent != "" {
			surface = "subagent"
		}
		newSession(src, id, surface, str(p, "cli_version"), parent)
		return nil, nil
	}
	if src.Session == nil {
		return nil, errors.New("missing_session_header")
	}
	native := str(p, "type")
	op := str(p, "turn_id")
	if k == "event_msg" {
		switch native {
		case "task_started", "turn_started":
			src.Parser.Operation = op
			return []Fact{observation("lifecycle.observed", native, op, t, EventData{Phase: "started", Scope: "agent_turn"})}, nil
		case "task_complete", "turn_complete", "turn_aborted":
			outcome := "completed"
			if native == "turn_aborted" {
				outcome = "interrupted"
			}
			if v := p["error"]; len(v) > 0 && string(v) != "null" {
				outcome = "failed"
			}
			data := EventData{Phase: "ended", Scope: "agent_turn"}
			if op != "" {
				data.Outcome = outcome
			}
			f := observation("lifecycle.observed", native, op, t, data)
			f.NativeReason = outcome
			return []Fact{f}, nil
		case "user_message":
			return []Fact{observation("input.observed", native, src.Parser.Operation, t, EventData{})}, nil
		case "agent_message", "agent_reasoning":
			return []Fact{observation("message.observed", native, src.Parser.Operation, t, EventData{Role: "assistant"})}, nil
		case "item_completed":
			item := obj(p["item"])
			if contains("commandExecution command_execution mcpToolCall mcp_tool_call functionCallOutput function_call_output", str(item, "type")) {
				return []Fact{observation("tool.observed", native, op, t, EventData{CallID: str(item, "id"), Phase: "finished"})}, nil
			}
		}
	} else if k == "response_item" {
		switch native {
		case "function_call", "custom_tool_call", "local_shell_call":
			return []Fact{observation("tool.observed", native, src.Parser.Operation, t, EventData{CallID: str(p, "call_id"), Phase: "requested"})}, nil
		case "function_call_output", "custom_tool_call_output":
			return []Fact{observation("tool.observed", native, "", t, EventData{CallID: str(p, "call_id"), Phase: "finished"})}, nil
		case "message":
			role := str(p, "role")
			if contains("user assistant system tool", role) {
				return []Fact{observation("message.observed", native, src.Parser.Operation, t, EventData{Role: role})}, nil
			}
		}
	}
	return nil, nil
}

func parseClaude(src *Source, o object) ([]Fact, error) {
	if id := str(o, "sessionId"); id != "" {
		parent := ""
		surface := "unknown"
		name := strings.TrimSuffix(filepath.Base(src.Path), ".jsonl")
		if strings.HasPrefix(name, "agent-") {
			parent, id, surface = id, name, "subagent"
		}
		if contains("cli desktop", str(o, "entrypoint")) {
			surface = str(o, "entrypoint")
		}
		if parent != "" {
			surface = "subagent"
		}
		newSession(src, id, surface, str(o, "version"), parent)
	}
	if src.Session == nil {
		return nil, nil
	}
	t := stamp(o["timestamp"])
	m := obj(o["message"])
	facts := []Fact{}
	native := str(o, "type")
	switch native {
	case "user":
		results := false
		for _, c := range array(m["content"]) {
			if str(c, "type") == "tool_result" {
				results = true
				outcome := "completed"
				if boolean(c, "is_error") {
					outcome = "failed"
				}
				facts = append(facts, observation("tool.observed", "tool_result", "", t, EventData{CallID: str(c, "tool_use_id"), Phase: "finished", Outcome: outcome}))
			}
		}
		if !results && !boolean(o, "isMeta") && !boolean(o, "isCompactSummary") && !boolean(o, "isVisibleInTranscriptOnly") {
			src.Parser.Operation = str(o, "uuid")
			facts = append(facts, observation("input.observed", native, src.Parser.Operation, t, EventData{MessageID: str(o, "uuid")}))
		}
	case "assistant":
		hasTools := false
		for _, c := range array(m["content"]) {
			if str(c, "type") == "tool_use" {
				hasTools = true
				facts = append(facts, observation("tool.observed", "tool_use", src.Parser.Operation, t, EventData{CallID: str(c, "id"), Phase: "requested"}))
			}
		}
		data := EventData{Role: "assistant", StopReason: str(m, "stop_reason"), MessageID: str(o, "uuid"), HasTools: &hasTools}
		if boolean(o, "isApiErrorMessage") {
			data.Outcome = "failed"
		}
		f := observation("message.observed", native, src.Parser.Operation, t, data)
		f.NativeReason = data.StopReason
		facts = append(facts, f)
	}
	return facts, nil
}

func parsePi(src *Source, o object) ([]Fact, error) {
	t := stamp(o["timestamp"])
	if str(o, "type") == "session" {
		src.Parser.Format = number(o, "version")
		if src.Parser.Format != 3 {
			return nil, errors.New("unsupported_pi_format")
		}
		newSession(src, str(o, "id"), "unknown", "", "")
		return nil, nil
	}
	if src.Session == nil {
		return nil, errors.New("missing_session_header")
	}
	facts := []Fact{}
	if id := str(o, "id"); id != "" {
		if parent := str(o, "parentId"); src.Parser.LastNode != "" && parent != src.Parser.LastNode {
			changed := true
			src.Parser.Operation = ""
			facts = append(facts, observation("session.observed", "persisted_branch_changed", "", t, EventData{MessageID: id, ParentNodeID: parent, BranchChanged: &changed}))
		}
		src.Parser.LastNode = id
	}
	if str(o, "type") != "message" {
		return facts, nil
	}
	m := obj(o["message"])
	switch str(m, "role") {
	case "user":
		src.Parser.Operation = str(o, "id")
		facts = append(facts, observation("input.observed", "user", src.Parser.Operation, t, EventData{MessageID: str(o, "id")}))
	case "toolResult":
		outcome := "completed"
		if boolean(m, "isError") {
			outcome = "failed"
		}
		facts = append(facts, observation("tool.observed", "toolResult", "", t, EventData{CallID: str(m, "toolCallId"), Phase: "finished", Outcome: outcome}))
	case "assistant":
		tools := false
		for _, c := range array(m["content"]) {
			if str(c, "type") == "toolCall" {
				tools = true
				facts = append(facts, observation("tool.observed", "toolCall", src.Parser.Operation, t, EventData{CallID: str(c, "id"), Phase: "requested"}))
			}
		}
		f := observation("message.observed", "assistant", src.Parser.Operation, t, EventData{Role: "assistant", StopReason: str(m, "stopReason"), MessageID: str(o, "id"), ParentNodeID: str(o, "parentId"), HasTools: &tools})
		f.NativeReason = f.Data.StopReason
		facts = append(facts, f)
	}
	return facts, nil
}

func parseDeepseek(src *Source, o object) ([]Fact, error) {
	k := str(o, "type")
	if k == "session" {
		src.Parser.Format = number(o, "version")
		if src.Parser.Format != 3 && src.Parser.Format != 4 {
			return nil, errors.New("unsupported_deepseek_format")
		}
		if src.Generation != src.Parser.Format {
			return nil, errors.New("generation_header_mismatch")
		}
		src.Parser.Seeded = boolean(o, "isSeeded")
		newSession(src, str(o, "id"), "unknown", "", "")
		return nil, nil
	}
	if src.Session == nil {
		return nil, errors.New("missing_session_header")
	}
	var seq int64
	if json.Unmarshal(o["seq"], &seq) != nil || seq < 0 || seq != src.Parser.NextSeq {
		return nil, errors.New("invalid_event_sequence")
	}
	src.Parser.NextSeq = seq + 1
	if k == "session/end-seed" {
		return nil, nil
	}
	if src.Parser.Seeded && (!src.Parser.SeedReady || seq <= src.Parser.SeedCut) {
		return nil, nil
	}
	d := obj(o["data"])
	t := stamp(o["time"])
	op := str(d, "turn")
	if op == "" && len(d["turn"]) > 0 {
		op = string(d["turn"])
	}
	if k == "turn/start" || k == "turn/end" {
		var turn int64
		if json.Unmarshal(d["turn"], &turn) != nil || turn < 0 || t.IsZero() {
			return nil, errors.New("invalid_turn_boundary")
		}
	}
	if op == "" {
		op = src.Parser.Operation
	}
	switch k {
	case "turn/start":
		src.Parser.Operation = op
		return []Fact{observation("lifecycle.observed", k, op, t, EventData{Phase: "started", Scope: "agent_turn"})}, nil
	case "step/start":
		return []Fact{observation("message.observed", k, op, t, EventData{Role: "assistant"})}, nil
	case "tool/call":
		return []Fact{observation("tool.observed", k, op, t, EventData{CallID: str(d, "callId"), Phase: "requested"})}, nil
	case "tool/result":
		facts := []Fact{}
		for _, r := range array(obj(d["message"])["content"]) {
			if str(r, "type") == "tool-result" {
				outcome := "completed"
				if boolean(r, "isError") {
					outcome = "failed"
				}
				facts = append(facts, observation("tool.observed", k, op, t, EventData{CallID: str(r, "toolCallId"), Phase: "finished", Outcome: outcome}))
			}
		}
		return facts, nil
	case "approval/asked":
		return []Fact{observation("approval.observed", k, op, t, EventData{RequestID: str(d, "id"), Phase: "requested"})}, nil
	case "approval/decided":
		decision := "unknown"
		reason := str(d, "outcome")
		if strings.HasPrefix(reason, "allowed") {
			decision = "allowed"
		} else if strings.HasPrefix(reason, "denied") {
			decision = "denied"
		}
		f := observation("approval.observed", k, op, t, EventData{RequestID: str(d, "id"), Phase: "resolved", Decision: decision})
		f.NativeReason = reason
		return []Fact{f}, nil
	case "turn/end":
		reason := str(obj(d["reason"]), "kind")
		outcome := "unknown"
		switch reason {
		case "completed":
			outcome = "completed"
		case "aborted", "interrupted":
			outcome = "interrupted"
		case "error", "blocked", "max-tokens":
			outcome = "failed"
		}
		f := observation("lifecycle.observed", k, op, t, EventData{Phase: "ended", Scope: "agent_turn", Outcome: outcome})
		f.NativeReason = reason
		f.Historical = reason == "interrupted" || reason == "forked"
		return []Fact{f}, nil
	}
	return nil, nil
}
