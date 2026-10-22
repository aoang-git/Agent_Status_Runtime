package monitor

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

var dataFields = map[string]string{
	"session.observed":   "surface agent_version parent_session_id message_id parent_node_id branch_changed",
	"input.observed":     "message_id",
	"message.observed":   "role stop_reason message_id parent_node_id has_tools outcome",
	"tool.observed":      "call_id phase outcome",
	"approval.observed":  "request_id phase decision",
	"lifecycle.observed": "phase scope outcome",
	"state.reported":     "state",
	"source.heartbeat":   "",
}

func contains(list, value string) bool {
	for _, candidate := range strings.Fields(list) {
		if candidate == value {
			return true
		}
	}
	return false
}

// Go's JSON decoder accepts duplicate and case-insensitive fields; the wire
// protocol deliberately does not.
func uniqueJSON(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	if delim, ok := t.(json.Delim); ok {
		if delim == '{' {
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				key := k.(string)
				if seen[key] {
					return errors.New("duplicate_field")
				}
				seen[key] = true
				if err = uniqueJSON(d); err != nil {
					return err
				}
			}
		} else if delim == '[' {
			for d.More() {
				if err = uniqueJSON(d); err != nil {
					return err
				}
			}
		} else {
			return errors.New("invalid_json")
		}
		_, err = d.Token()
		return err
	}
	return nil
}
func DecodeInput(b []byte) (InputEvent, error) {
	var e InputEvent
	if !utf8.Valid(b) {
		return e, errors.New("invalid_json")
	}
	if len(b) > MaxInput {
		return e, errors.New("body_too_large")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	if err := uniqueJSON(d); err != nil {
		return e, errors.New("invalid_json")
	}
	if _, err := d.Token(); err != io.EOF {
		return e, errors.New("invalid_json")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil || fields == nil {
		return e, errors.New("invalid_event")
	}
	allowed := "schema producer_id event_id agent session_namespace session_id operation_id occurred_at kind data native_type native_reason reported_basis stream_id source_sequence"
	for k, v := range fields {
		if !contains(allowed, k) || string(v) == "null" || string(v) == `""` {
			return e, errors.New("invalid_field")
		}
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&e) != nil {
		return e, errors.New("invalid_field")
	}
	if e.Schema != 2 {
		return e, errors.New("unsupported_schema")
	}
	list, ok := dataFields[e.Kind]
	if !ok {
		return e, errors.New("unsupported_kind")
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(fields["data"], &data) != nil || data == nil {
		return e, errors.New("invalid_data")
	}
	for k, v := range data {
		if !contains(list, k) || string(v) == "null" || string(v) == `""` {
			return e, errors.New("invalid_data_field")
		}
	}
	for _, s := range []string{e.ProducerID, e.EventID, e.Agent, e.SessionID} {
		if !validText(s) {
			return e, errors.New("missing_or_invalid_identity")
		}
	}
	if e.Namespace == "" {
		e.Namespace = e.ProducerID
	}
	for _, s := range []string{e.Namespace, e.OperationID, e.NativeType, e.NativeReason, e.StreamID, e.Data.Surface, e.Data.AgentVersion, e.Data.ParentSessionID, e.Data.Role, e.Data.StopReason, e.Data.CallID, e.Data.RequestID, e.Data.MessageID, e.Data.ParentNodeID, e.Data.Decision} {
		if s != "" && !validText(s) {
			return e, errors.New("invalid_metadata")
		}
	}
	if e.ReportedBasis == "" {
		e.ReportedBasis = "self_report"
	}
	if !contains("native_event inferred self_report", e.ReportedBasis) {
		return e, errors.New("invalid_basis")
	}
	if (e.SourceSequence != nil && (*e.SourceSequence < 0 || e.StreamID == "")) || (e.StreamID != "" && e.SourceSequence == nil) {
		return e, errors.New("invalid_sequence")
	}
	if e.OccurredAt != nil && e.OccurredAt.IsZero() {
		return e, errors.New("invalid_time")
	}
	if e.Data.Surface != "" && !contains("cli desktop subagent unknown", e.Data.Surface) {
		return e, errors.New("invalid_surface")
	}
	if e.Data.Outcome != "" && !contains("completed interrupted failed unknown", e.Data.Outcome) {
		return e, errors.New("invalid_outcome")
	}
	switch e.Kind {
	case "message.observed":
		if !contains("user assistant system tool", e.Data.Role) {
			return e, errors.New("invalid_role")
		}
	case "tool.observed":
		if e.Data.CallID == "" || !contains("requested started finished", e.Data.Phase) {
			return e, errors.New("invalid_tool")
		}
	case "approval.observed":
		if e.Data.RequestID == "" || !contains("requested resolved", e.Data.Phase) || (e.Data.Decision != "" && !contains("allowed denied unknown", e.Data.Decision)) {
			return e, errors.New("invalid_approval")
		}
	case "lifecycle.observed":
		if !contains("started ended", e.Data.Phase) || !contains("message agent_turn agent_run session", e.Data.Scope) {
			return e, errors.New("invalid_lifecycle")
		}
		if e.Data.Phase == "started" && e.Data.Outcome != "" {
			return e, errors.New("invalid_outcome")
		}
		if e.Data.Outcome != "" && e.Data.Outcome != "unknown" && e.OperationID == "" {
			return e, errors.New("operation_required")
		}
	case "state.reported":
		if !contains("working tool_activity waiting_confirmation idle completed interrupted failed unknown offline", e.Data.State) {
			return e, errors.New("invalid_state")
		}
		if contains("completed interrupted failed", e.Data.State) && e.OperationID == "" {
			return e, errors.New("operation_required")
		}
	}
	return e, nil
}
func validText(s string) bool {
	return s != "" && utf8.RuneCountInString(s) <= 256 && !strings.ContainsFunc(s, unicode.IsControl)
}
