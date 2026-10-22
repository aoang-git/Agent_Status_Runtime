package monitor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

var now = time.Now().UTC().Truncate(time.Millisecond)

func ts(n int) string { return now.Add(time.Duration(n) * time.Millisecond).Format(time.RFC3339Nano) }
func codex(kind, turn string, n int) string {
	return `{"type":"event_msg","timestamp":"` + ts(n) + `","payload":{"type":"` + kind + `","turn_id":"` + turn + `"}}` + "\n"
}
func header(id string) string {
	return `{"type":"session_meta","payload":{"id":"` + id + `","originator":"Codex Desktop","cli_version":"sample","source":"cli"}}` + "\n"
}
func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
}
func appendFile(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.Write(b); err != nil {
		t.Fatal(err)
	}
}
func fixtureEngine(t *testing.T, agent string) (*Engine, string, string) {
	t.Helper()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "state.sqlite")
	s, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e, err := NewEngine(s, []Root{{agent, root}}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return e, root, db
}
func scan(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.Scan(); err != nil {
		t.Fatal(err)
	}
}
func one(t *testing.T, e *Engine) *Session {
	t.Helper()
	s, err := e.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Sessions) != 1 {
		t.Fatalf("want one session, got %d", len(s.Sessions))
	}
	return s.Sessions[0]
}
func ended(t *testing.T, e *Engine) []Event {
	t.Helper()
	events, _, err := e.Store.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	result := []Event{}
	for _, v := range events {
		if v.Type == "turn.ended" {
			result = append(result, v)
		}
	}
	return result
}

func TestIncrementalPartialRestartAndHistoricalSilence(t *testing.T) {
	e, root, db := fixtureEngine(t, "codex")
	path := filepath.Join(root, "rollout.jsonl")
	write(t, path, header("one")+codex("task_started", "old", 0)+codex("task_complete", "old", 1))
	scan(t, e)
	if one(t, e).State != "idle" || len(ended(t, e)) != 0 {
		t.Fatal("historical completion notified")
	}
	appendFile(t, path, []byte(codex("task_started", "new", 2)))
	scan(t, e)
	line := codex("task_complete", "new", 3)
	appendFile(t, path, []byte(line[:len(line)-2]))
	scan(t, e)
	if one(t, e).State != "working" {
		t.Fatal("partial line changed state")
	}
	appendFile(t, path, []byte(line[len(line)-2:]))
	scan(t, e)
	scan(t, e)
	if len(ended(t, e)) != 1 || one(t, e).Outcome != "completed" {
		t.Fatal("live completion not exactly once")
	}
	appendFile(t, path, []byte(codex("task_started", "offline", 4)+codex("task_complete", "offline", 5)))
	e.Store.Close()
	s, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	restarted, err := NewEngine(s, e.Roots, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	scan(t, restarted)
	if len(ended(t, restarted)) != 1 || one(t, restarted).TurnID != "offline" {
		t.Fatal("restart replayed recovered completion")
	}
}

func TestOldTurnCannotEndNewTurnAndTerminalErrors(t *testing.T) {
	e, root, _ := fixtureEngine(t, "codex")
	p := filepath.Join(root, "one.jsonl")
	write(t, p, header("one")+codex("task_started", "a", 0))
	scan(t, e)
	appendFile(t, p, []byte(codex("task_started", "b", 1)+codex("task_complete", "a", 2)))
	scan(t, e)
	if one(t, e).TurnID != "b" || one(t, e).State != "working" || len(ended(t, e)) != 0 {
		t.Fatal("old terminal closed current turn")
	}
	appendFile(t, p, []byte(codex("task_complete", "", 2)))
	scan(t, e)
	if len(ended(t, e)) != 0 || one(t, e).State != "unknown" {
		t.Fatal("unattributed completion ended current turn")
	}
	line := `{"type":"event_msg","timestamp":"` + ts(3) + `","payload":{"type":"task_complete","turn_id":"b","error":{"message":"DO_NOT_STORE_THIS"}}}` + "\n"
	appendFile(t, p, []byte(line))
	scan(t, e)
	if one(t, e).Outcome != "failed" || len(ended(t, e)) != 1 {
		t.Fatal("terminal error misclassified")
	}
	rows, err := e.Store.db.Query(`SELECT data FROM sources UNION ALL SELECT data FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		rows.Scan(&b)
		if bytes.Contains(b, []byte("DO_NOT_STORE_THIS")) {
			t.Fatal("content leaked into storage")
		}
	}
}

func TestDeepseekSeedCutAndNestedToolResult(t *testing.T) {
	e, root, _ := fixtureEngine(t, "deepseek")
	p := filepath.Join(root, "session.v3.jsonl")
	h := `{"type":"session","version":3,"id":"child","isSeeded":true}` + "\n"
	seed := dsh("session/end-seed", `{"inherited":true}`, 0) + dsh("turn/start", `{"turn":5}`, 1) + dsh("turn/end", `{"turn":5,"reason":{"kind":"completed"}}`, 2) + dsh("session/end-seed", `{"inherited":true}`, 3)
	write(t, p, h+seed)
	scan(t, e)
	if one(t, e).TurnID != "" || len(ended(t, e)) != 0 {
		t.Fatal("inherited events became child activity")
	}
	appendFile(t, p, []byte(dsh("turn/start", `{"turn":6}`, 4)+dsh("tool/call", `{"turn":6,"callId":"c"}`, 5)))
	scan(t, e)
	if len(one(t, e).Tools) != 1 {
		t.Fatal("missing tool call")
	}
	appendFile(t, p, []byte(dsh("tool/result", `{"turn":6,"message":{"role":"user","content":[{"type":"tool-result","toolCallId":"c","isError":true}]}}`, 6)))
	scan(t, e)
	if len(one(t, e).Tools) != 0 || one(t, e).State != "working" {
		t.Fatal("nested tool result was not associated")
	}
	appendFile(t, p, []byte(dsh("turn/end", `{"turn":6,"reason":{"kind":"completed"}}`, 7)))
	scan(t, e)
	if len(ended(t, e)) != 1 {
		t.Fatal("child completion not emitted after seed cut")
	}
}

func TestClaudeSyntheticInputDoesNotClearToolState(t *testing.T) {
	e, root, _ := fixtureEngine(t, "claude")
	p := filepath.Join(root, "one.jsonl")
	write(t, p, `{"type":"user","sessionId":"one","uuid":"u","timestamp":"`+ts(0)+`","message":{"content":"user"}}`+"\n"+`{"type":"assistant","sessionId":"one","timestamp":"`+ts(1)+`","message":{"content":[{"type":"tool_use","id":"c"}]}}`+"\n")
	scan(t, e)
	appendFile(t, p, []byte(`{"type":"user","sessionId":"one","isCompactSummary":true,"uuid":"summary","timestamp":"`+ts(2)+`","message":{"content":"synthetic"}}`+"\n"))
	scan(t, e)
	if one(t, e).TurnID != "u" || len(one(t, e).Tools) != 1 {
		t.Fatal("synthetic input became user turn")
	}
}

func TestCheckpointAndEventRollbackTogether(t *testing.T) {
	e, root, _ := fixtureEngine(t, "codex")
	p := filepath.Join(root, "one.jsonl")
	write(t, p, header("one")+codex("task_started", "a", 0))
	scan(t, e)
	if _, err := e.Store.db.Exec(`CREATE TRIGGER fail_events BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	appendFile(t, p, []byte(codex("task_complete", "a", 1)))
	if e.Scan() == nil {
		t.Fatal("expected commit failure")
	}
	if one(t, e).State != "working" || len(ended(t, e)) != 0 {
		t.Fatal("failed transaction advanced memory or events")
	}
	if _, err := e.Store.db.Exec(`DROP TRIGGER fail_events`); err != nil {
		t.Fatal(err)
	}
	scan(t, e)
	if len(ended(t, e)) != 1 {
		t.Fatal("rolled-back record was lost")
	}
}

func TestParserRevisionRebuildsCheckpointSilently(t *testing.T) {
	e, root, _ := fixtureEngine(t, "deepseek")
	p := filepath.Join(root, "session.v3.jsonl")
	write(t, p, `{"type":"session","version":3,"id":"one","isSeeded":false}`+"\n"+dsh("turn/start", `{"turn":0}`, 0))
	scan(t, e)
	for _, src := range e.sources {
		src.ParserRevision = 0
		src.Parser.NextSeq = 0
		if err := e.Store.Commit(src, nil); err != nil {
			t.Fatal(err)
		}
	}
	appendFile(t, p, []byte(dsh("turn/end", `{"turn":0,"reason":{"kind":"completed"}}`, 1)))
	scan(t, e)
	if one(t, e).SourceHealth != "healthy" || len(ended(t, e)) != 0 {
		t.Fatal("parser upgrade failed to rebase silently")
	}
	appendFile(t, p, []byte(dsh("turn/start", `{"turn":1}`, 2)+dsh("turn/end", `{"turn":1,"reason":{"kind":"completed"}}`, 3)))
	scan(t, e)
	if len(ended(t, e)) != 1 {
		t.Fatal("new parser did not resume live tail")
	}
}

func TestRebuildAndCorruptionIsolation(t *testing.T) {
	e, root, _ := fixtureEngine(t, "codex")
	p := filepath.Join(root, "one.jsonl")
	write(t, p, header("one")+codex("task_started", "a", 0))
	scan(t, e)
	write(t, p, header("one")+codex("task_started", "b", 1)+codex("task_complete", "b", 2))
	scan(t, e)
	if one(t, e).Outcome != "completed" || len(ended(t, e)) != 0 {
		t.Fatal("rewrite emitted history")
	}
	appendFile(t, p, []byte("not-json\n"))
	scan(t, e)
	if one(t, e).SourceHealth != "degraded" || one(t, e).State != "unknown" {
		t.Fatal("invalid source not degraded")
	}
	write(t, filepath.Join(root, "two.jsonl"), header("two")+codex("task_started", "c", 3))
	scan(t, e)
	s, _ := e.Snapshot()
	if len(s.Sessions) != 2 {
		t.Fatal("bad source blocked discovery")
	}
}

func frame(t *testing.T, s string) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return enc.EncodeAll([]byte(s), nil)
}
func dsh(kind, data string, n int) string {
	return `{"type":"` + kind + `","seq":` + jsonNumber(n) + `,"time":` + jsonNumber64(now.UnixMilli()+int64(n)) + `,"data":` + data + "}\n"
}
func jsonNumber(n int) string     { b, _ := json.Marshal(n); return string(b) }
func jsonNumber64(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestDeepseekFramesApprovalAndRecovery(t *testing.T) {
	e, root, _ := fixtureEngine(t, "deepseek")
	p := filepath.Join(root, "project", "session", "session.v3.jsonl.zstd")
	h := `{"type":"session","version":3,"id":"dsh","isSeeded":false}` + "\n"
	write(t, p, "")
	appendFile(t, p, frame(t, h))
	scan(t, e)
	b := frame(t, dsh("turn/start", `{"turn":0}`, 0)+dsh("approval/asked", `{"id":"a","callId":"c"}`, 1)+dsh("approval/decided", `{"id":"a","outcome":"allowed-once"}`, 2))
	appendFile(t, p, b[:len(b)-1])
	scan(t, e)
	if one(t, e).TurnID != "" {
		t.Fatal("partial frame was committed")
	}
	appendFile(t, p, b[len(b)-1:])
	scan(t, e)
	if one(t, e).State != "working" || len(one(t, e).Approvals) != 0 {
		t.Fatal("automatic approval remained pending")
	}
	appendFile(t, p, frame(t, dsh("turn/end", `{"turn":0,"reason":{"kind":"interrupted"}}`, 3)))
	scan(t, e)
	if one(t, e).Outcome != "interrupted" || len(ended(t, e)) != 0 {
		t.Fatal("recovery closer emitted live notification")
	}
	appendFile(t, p, frame(t, dsh("turn/start", `{"turn":1}`, 4)+dsh("turn/end", `{"turn":1,"reason":{"kind":"blocked"}}`, 5)))
	scan(t, e)
	if one(t, e).Outcome != "failed" || len(ended(t, e)) != 1 {
		t.Fatal("blocked turn must end as failure")
	}
	bad := frame(t, dsh("turn/start", `{"turn":2}`, 6))
	bad[len(bad)-1] ^= 0xff
	appendFile(t, p, bad)
	scan(t, e)
	if one(t, e).SourceHealth != "degraded" || one(t, e).TurnID != "1" {
		t.Fatal("bad checksum advanced parser")
	}
}

func TestDeepseekNewestUnknownGenerationAndAmbiguousEncoding(t *testing.T) {
	e, root, _ := fixtureEngine(t, "deepseek")
	dir := filepath.Join(root, "project", "id")
	write(t, filepath.Join(dir, "session.v3.jsonl"), `{"type":"session","version":3,"id":"one"}`+"\n")
	write(t, filepath.Join(dir, "session.v9.jsonl"), `{"type":"session","version":9,"id":"one"}`+"\n")
	scan(t, e)
	if len(e.sources) != 1 {
		t.Fatal("read an obsolete generation")
	}
	for _, s := range e.sources {
		if s.Health != "degraded" || s.Reason != "unsupported_deepseek_format" {
			t.Fatalf("unexpected source: %+v", s)
		}
	}
	write(t, filepath.Join(dir, "session.v9.jsonl.zstd"), "")
	scan(t, e)
	for _, s := range e.sources {
		if s.Health != "degraded" && s.Health != "unavailable" {
			t.Fatal("ambiguous encoding accepted")
		}
	}
}

func TestClaudeAndPiDoNotInventCompletion(t *testing.T) {
	for _, agent := range []string{"claude", "pi"} {
		t.Run(agent, func(t *testing.T) {
			e, root, _ := fixtureEngine(t, agent)
			p := filepath.Join(root, "session.jsonl")
			if agent == "claude" {
				write(t, p, `{"type":"user","sessionId":"one","entrypoint":"cli","version":"sample","uuid":"u","timestamp":"`+ts(0)+`","message":{"role":"user","content":"SECRET_PROMPT"}}`+"\n")
				scan(t, e)
				appendFile(t, p, []byte(`{"type":"assistant","sessionId":"one","timestamp":"`+ts(1)+`","message":{"role":"assistant","stop_reason":"end_turn","content":[]}}`+"\n"))
			} else {
				write(t, p, `{"type":"session","version":3,"id":"one"}`+"\n"+`{"type":"message","id":"u","parentId":null,"timestamp":"`+ts(0)+`","message":{"role":"user","content":"SECRET_PROMPT"}}`+"\n")
				scan(t, e)
				appendFile(t, p, []byte(`{"type":"message","id":"a","parentId":"u","timestamp":"`+ts(1)+`","message":{"role":"assistant","stopReason":"stop","content":[]}}`+"\n"))
			}
			scan(t, e)
			if len(ended(t, e)) != 0 || one(t, e).State != "unknown" {
				t.Fatal("message ending promoted to completion")
			}
			if agent == "claude" && (one(t, e).Surface != "cli" || one(t, e).AgentVersion != "sample") {
				t.Fatal("sparse transcript erased identity metadata")
			}
		})
	}
}

func TestReducerConcurrentCallsAndLateTerminals(t *testing.T) {
	s := &Session{}
	apply(s, Fact{Kind: "start", Turn: "a", At: now})
	apply(s, Fact{Kind: "tool", Call: "one", At: now})
	apply(s, Fact{Kind: "tool", Call: "two", At: now})
	apply(s, Fact{Kind: "tool_end", Call: "one", At: now})
	if s.State != "tool_activity" || len(s.Tools) != 1 {
		t.Fatal("parallel call was cleared")
	}
	apply(s, Fact{Kind: "end", Turn: "a", Outcome: "completed", At: now})
	apply(s, Fact{Kind: "start", Turn: "a", At: now})
	if s.State != "idle" {
		t.Fatal("duplicate start reopened a completed turn")
	}
}

func TestLeaseAndAuthentication(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.sqlite")
	release, err := AcquireLease(db)
	if err != nil {
		t.Fatal(err)
	}
	if release2, err := AcquireLease(db); err == nil {
		release2()
		t.Fatal("second writer allowed")
	}
	release()
	release, err = AcquireLease(db)
	if err != nil {
		t.Fatal(err)
	}
	release()
	e, _, _ := fixtureEngine(t, "codex")
	server := httptest.NewServer(Handler(e, "token"))
	defer server.Close()
	resp, err := http.Get(server.URL + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("unauthenticated access allowed")
	}
	req, _ := http.NewRequest("GET", server.URL+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("authenticated request failed")
	}
}

func TestSSESnapshotReplayAndReset(t *testing.T) {
	e, root, _ := fixtureEngine(t, "codex")
	p := filepath.Join(root, "one.jsonl")
	write(t, p, header("one")+codex("task_started", "a", 0))
	scan(t, e)
	snap, _ := e.Snapshot()
	appendFile(t, p, []byte(codex("task_complete", "a", 1)))
	scan(t, e)
	server := httptest.NewServer(Handler(e, "token"))
	defer server.Close()
	for _, test := range []struct{ cursor, want string }{{"", "snapshot"}, {snap.Epoch + ":" + jsonNumber64(snap.Revision), "turn.ended"}, {"wrong:0", "reset"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/v1/events", nil)
		req.Header.Set("Authorization", "Bearer token")
		req.Header.Set("Last-Event-ID", test.cursor)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		r := bufio.NewReader(resp.Body)
		var event string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				resp.Body.Close()
				cancel()
				t.Fatal(err)
			}
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			}
			if line == "\n" {
				break
			}
		}
		resp.Body.Close()
		cancel()
		if event != test.want {
			t.Fatalf("want %s got %s", test.want, event)
		}
	}
}

func TestFrameReaderNeverAcceptsPartialOrUncheckedFrames(t *testing.T) {
	b := frame(t, "{}\n")
	for n := 0; n < len(b); n++ {
		if _, err := frameSize(b[:n]); err == nil {
			t.Fatalf("accepted partial frame at %d", n)
		}
	}
	if n, err := frameSize(b); err != nil || n != len(b) {
		t.Fatalf("full frame failed %d %v", n, err)
	}
	p := filepath.Join(t.TempDir(), "x.jsonl")
	write(t, p, strings.Repeat("x", maxLine+1))
	f, _ := os.Open(p)
	defer f.Close()
	if _, err := readUnits(f, 0, maxLine+1); err == nil {
		t.Fatal("unbounded line accepted")
	}
}

func TestWatcherDiscoversNewDirectory(t *testing.T) {
	e, root, _ := fixtureEngine(t, "codex")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx, 250*time.Millisecond) }()
	write(t, filepath.Join(root, "new", "rollout.jsonl"), header("one")+codex("task_started", "a", 0))
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		s, err := e.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Sessions) == 1 {
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("new directory not discovered")
}

func TestStaleEvidencePublishesUnknownWithoutCompletion(t *testing.T) {
	e, root, _ := fixtureEngine(t, "codex")
	write(t, filepath.Join(root, "one.jsonl"), header("one")+codex("task_started", "a", 0))
	scan(t, e)
	if one(t, e).State != "working" {
		t.Fatal("fresh start missing")
	}
	_, before, _ := e.Store.Events(0)
	e.Stale = time.Nanosecond
	scan(t, e)
	events, _, err := e.Store.Events(before)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "state.changed" || events[0].Session.State != "unknown" || len(ended(t, e)) != 0 {
		t.Fatal("staleness did not publish a conservative change")
	}
}

func TestEventRetentionRequiresReset(t *testing.T) {
	e, root, _ := fixtureEngine(t, "codex")
	write(t, filepath.Join(root, "one.jsonl"), header("one"))
	scan(t, e)
	s := one(t, e)
	var src *Source
	for _, v := range e.sources {
		src = v
	}
	events := make([]Event, eventLimit+2)
	for i := range events {
		events[i] = Event{Type: "state.changed", Session: s}
	}
	if err := e.Store.Commit(src, events); err != nil {
		t.Fatal(err)
	}
	retained, _, err := e.Store.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(retained) != eventLimit {
		t.Fatal("event retention is unbounded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "http://localhost/v1/events", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Last-Event-ID", e.Store.Epoch+":0")
	server := httptest.NewServer(Handler(e, "token"))
	defer server.Close()
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(server.URL, "http://")
	req.RequestURI = ""
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	found := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "event: reset\n" {
			found = true
		}
		if line == "\n" {
			break
		}
	}
	if !found {
		t.Fatal("expired cursor did not reset")
	}
}
