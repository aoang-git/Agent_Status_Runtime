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
func boolean(o object, k string) bool  { var v bool; _ = json.Unmarshal(o[k], &v); return v }
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

// Parse only copies whitelisted metadata. Errors never include raw log content.
func parse(src *Source, line []byte) ([]Fact, error) {
	if !json.Valid(line) {
		return nil, errors.New("invalid_json")
	}
	o := obj(line)
	if o == nil || str(o, "type") == "" {
		return nil, errors.New("invalid_record_shape")
	}
	switch src.Root.Agent {
	case "codex":
		return parseCodex(src, o)
	case "claude":
		return parseClaude(src, o)
	case "pi":
		return parsePi(src, o)
	case "deepseek":
		return parseDeepseek(src, o)
	}
	return nil, errors.New("unsupported_agent")
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
		source, origin := str(p, "source"), str(p, "originator")
		if strings.Contains(strings.ToLower(origin), "desktop") {
			surface = "desktop"
		} else if source == "cli" || source == "exec" {
			surface = "cli"
		}
		parent := str(p, "parent_thread_id")
		if parent != "" {
			surface = "subagent"
		}
		newSession(src, id, surface, str(p, "cli_version"), parent)
		return nil, nil
	}
	f := Fact{At: t, Turn: str(p, "turn_id"), Reason: "codex_" + str(p, "type")}
	if k == "event_msg" {
		switch str(p, "type") {
		case "task_started", "turn_started":
			f.Kind = "start"
		case "task_complete", "turn_complete":
			if f.Turn == "" {
				f.Kind, f.Reason = "candidate", "unattributed_completion"
				break
			}
			f.Kind, f.Outcome = "end", "completed"
			if v := p["error"]; len(v) > 0 && string(v) != "null" {
				f.Outcome = "failed"
			}
		case "turn_aborted":
			if f.Turn == "" {
				f.Kind, f.Reason = "candidate", "unattributed_interruption"
			} else {
				f.Kind, f.Outcome = "end", "interrupted"
			}
		case "item_completed":
			i := obj(p["item"])
			switch str(i, "type") {
			case "commandExecution", "command_execution", "mcpToolCall", "mcp_tool_call", "functionCallOutput", "function_call_output":
				f.Kind, f.Call = "tool_end", str(i, "id")
			}
		}
	} else if k == "response_item" {
		switch str(p, "type") {
		case "function_call", "custom_tool_call", "local_shell_call":
			f.Kind, f.Call, f.Turn = "tool", str(p, "call_id"), ""
		case "function_call_output", "custom_tool_call_output":
			f.Kind, f.Call, f.Turn = "tool_end", str(p, "call_id"), ""
		}
	}
	return []Fact{f}, nil
}

func parseClaude(src *Source, o object) ([]Fact, error) {
	id := str(o, "sessionId")
	if id != "" {
		parent := ""
		surface := "unknown"
		name := strings.TrimSuffix(filepath.Base(src.Path), ".jsonl")
		if strings.HasPrefix(name, "agent-") {
			parent, id, surface = id, name, "subagent"
		}
		switch str(o, "entrypoint") {
		case "cli":
			surface = "cli"
		case "desktop":
			surface = "desktop"
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
	switch str(o, "type") {
	case "user":
		toolResults := false
		for _, c := range array(m["content"]) {
			if str(c, "type") == "tool_result" {
				toolResults = true
				facts = append(facts, Fact{Kind: "tool_end", Call: str(c, "tool_use_id"), At: t, Reason: "claude_tool_result"})
			}
		}
		if !toolResults && !boolean(o, "isMeta") && !boolean(o, "isCompactSummary") && !boolean(o, "isVisibleInTranscriptOnly") {
			facts = append(facts, Fact{Kind: "start", Turn: str(o, "uuid"), At: t, Reason: "claude_user_message"})
		}
	case "assistant":
		if boolean(o, "isApiErrorMessage") {
			return []Fact{{Kind: "candidate", At: t, Reason: "claude_api_error_message"}}, nil
		}
		for _, c := range array(m["content"]) {
			if str(c, "type") == "tool_use" {
				facts = append(facts, Fact{Kind: "tool", Call: str(c, "id"), At: t, Reason: "claude_tool_call"})
			}
		}
		if len(facts) == 0 {
			facts = append(facts, Fact{Kind: "activity", At: t, Reason: "claude_assistant_message"})
		}
		if stop := str(m, "stop_reason"); stop == "end_turn" || stop == "stop_sequence" || stop == "max_tokens" {
			facts = append(facts, Fact{Kind: "candidate", At: t, Reason: "message_end_without_turn_boundary"})
		}
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
			facts = append(facts, Fact{Kind: "branch", At: t, Reason: "persisted_branch_changed"})
		}
		src.Parser.LastNode = id
	}
	if str(o, "type") != "message" {
		return facts, nil
	}
	m := obj(o["message"])
	switch str(m, "role") {
	case "user":
		facts = append(facts, Fact{Kind: "start", Turn: str(o, "id"), At: t, Reason: "pi_user_message"})
	case "toolResult":
		facts = append(facts, Fact{Kind: "tool_end", Call: str(m, "toolCallId"), At: t, Reason: "pi_tool_result"})
	case "assistant":
		count := len(facts)
		for _, c := range array(m["content"]) {
			if str(c, "type") == "toolCall" {
				facts = append(facts, Fact{Kind: "tool", Call: str(c, "id"), At: t, Reason: "pi_tool_call"})
			}
		}
		if len(facts) == count {
			facts = append(facts, Fact{Kind: "candidate", At: t, Reason: "message_end_without_settled_event"})
		}
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
	f := Fact{At: stamp(o["time"]), Turn: str(d, "turn"), Reason: "deepseek_" + k}
	// Turn identifiers are numeric in some released formats.
	if f.Turn == "" && len(d["turn"]) > 0 {
		f.Turn = string(d["turn"])
	}
	if k == "turn/start" || k == "turn/end" {
		var turn int64
		if json.Unmarshal(d["turn"], &turn) != nil || turn < 0 || f.At.IsZero() {
			return nil, errors.New("invalid_turn_boundary")
		}
	}
	switch k {
	case "turn/start":
		f.Kind = "start"
	case "step/start":
		f.Kind = "activity"
	case "tool/call":
		f.Kind, f.Call = "tool", str(d, "callId")
	case "tool/result":
		// The persisted result is a user message containing tool-result blocks,
		// not a top-level callId. There may be more than one result block.
		facts := []Fact{}
		for _, result := range array(obj(d["message"])["content"]) {
			if str(result, "type") == "tool-result" {
				end := f
				end.Kind, end.Call = "tool_end", str(result, "toolCallId")
				facts = append(facts, end)
			}
		}
		return facts, nil
	case "approval/asked":
		f.Kind, f.Call = "approval", str(d, "id")
	case "approval/decided":
		f.Kind, f.Call = "approval_end", str(d, "id")
	case "turn/end":
		f.Kind, f.Outcome = "end", "unknown"
		f.Reason = "deepseek_unknown_end_reason"
		switch reason := str(obj(d["reason"]), "kind"); reason {
		case "completed":
			f.Outcome, f.Reason = "completed", "deepseek_completed"
		case "aborted":
			f.Outcome, f.Reason = "interrupted", "deepseek_aborted"
		case "error", "blocked", "max-tokens":
			f.Outcome, f.Reason = "failed", "deepseek_"+reason
		case "interrupted":
			f.Outcome, f.Silent = "interrupted", true
			f.Reason = "deepseek_recovery_interrupted"
		case "forked":
			f.Outcome, f.Silent = "unknown", true
			f.Reason = "deepseek_forked"
		}
	}
	return []Fact{f}, nil
}
