package monitor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

var now = time.Now().UTC().Truncate(time.Millisecond)

func ts(n int) string { return now.Add(time.Duration(n) * time.Millisecond).Format(time.RFC3339Nano) }
func codex(kind, turn string, n int) string {
	return `{"type":"event_msg","timestamp":"` + ts(n) + `","payload":{"type":"` + kind + `","turn_id":"` + turn + `"}}` + "\n"
}
func header(id string) string {
	return `{"type":"session_meta","payload":{"id":"` + id + `","originator":"Codex Desktop","cli_version":"sample","source":"cli"}}` + "\n"
}
func dsh(kind, data string, seq int) string {
	return fmt.Sprintf("{\"type\":%q,\"seq\":%d,\"time\":%q,\"data\":%s}\n", kind, seq, ts(seq), data)
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
	db := filepath.Join(t.TempDir(), "facts.sqlite")
	s, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e, err := NewEngine(s, []Root{{agent, root}})
	if err != nil {
		t.Fatal(err)
	}
	now = time.Now().UTC()
	return e, root, db
}
func scan(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.Scan(); err != nil {
		t.Fatal(err)
	}
}
func all(t *testing.T, e *Engine) []Fact {
	t.Helper()
	f, _, err := e.Store.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func liveEnds(t *testing.T, e *Engine) []Fact {
	result := []Fact{}
	for _, f := range all(t, e) {
		if f.Kind == "lifecycle.observed" && f.Data.Phase == "ended" && !f.Historical {
			result = append(result, f)
		}
	}
	return result
}
func input(id string) InputEvent {
	return InputEvent{Schema: 2, ProducerID: "adapter", EventID: id, Agent: "new-agent", Namespace: "local", SessionID: "s", OperationID: "op", Kind: "state.reported", ReportedBasis: "native_event", Data: EventData{State: "completed"}}
}
func frame(t *testing.T, s string) []byte {
	t.Helper()
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true))
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	return encoder.EncodeAll([]byte(s), nil)
}

func TestProtocolSchemaAndReceiverShareFixtures(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat = true
	if err := compiler.AddResource("schema.json", bytes.NewReader(SchemaJSON())); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Valid, Invalid []struct {
			Name  string
			Event json.RawMessage
		}
	}
	if err = json.Unmarshal(ProtocolSamples(), &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, group := range []struct {
		cases []struct {
			Name  string
			Event json.RawMessage
		}
		valid bool
	}{{fixtures.Valid, true}, {fixtures.Invalid, false}} {
		for _, sample := range group.cases {
			t.Run(sample.Name, func(t *testing.T) {
				var v any
				_ = json.Unmarshal(sample.Event, &v)
				_, err := DecodeInput(sample.Event)
				if (err == nil) != group.valid {
					t.Fatalf("receiver: %v", err)
				}
				if err = schema.Validate(v); (err == nil) != group.valid {
					t.Fatalf("schema: %v", err)
				}
			})
		}
	}
	for _, bad := range []string{`{"schema":2,"schema":1}`, `{"Schema":2}`, `[]`, `{} {}`, `{"kind":null}`} {
		if _, err := DecodeInput([]byte(bad)); err == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
	if _, err := DecodeInput([]byte(strings.Repeat("x", MaxInput+1))); err == nil {
		t.Fatal("unbounded body")
	}
}

func TestPublishedOutputAndInterpretationSamplesConformToSchema(t *testing.T) {
	c := jsonschema.NewCompiler()
	c.AssertFormat = true
	if err := c.AddResource("schema.json", bytes.NewReader(SchemaJSON())); err != nil {
		t.Fatal(err)
	}
	factSchema, err := c.Compile("schema.json#/$defs/fact")
	if err != nil {
		t.Fatal(err)
	}
	snapshotSchema, err := c.Compile("schema.json#/$defs/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	e, root, _ := fixtureEngine(t, "codex")
	write(t, filepath.Join(root, "x.jsonl"), header("one")+codex("task_started", "op", 0))
	scan(t, e)
	if _, _, err = e.Store.Ingest(context.Background(), input("external")); err != nil {
		t.Fatal(err)
	}
	validate := func(schema *jsonschema.Schema, value any) {
		b, _ := json.Marshal(value)
		var v any
		_ = json.Unmarshal(b, &v)
		if err := schema.Validate(v); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range all(t, e) {
		validate(factSchema, f)
	}
	snapshot, _ := e.Snapshot()
	validate(snapshotSchema, snapshot)
	b, _ := protocolFiles.ReadFile("protocol/interpretation.json")
	var cases struct{ Cases []struct{ Facts []Fact } }
	if json.Unmarshal(b, &cases) != nil {
		t.Fatal("invalid public rule samples")
	}
	for _, sample := range cases.Cases {
		for _, f := range sample.Facts {
			validate(factSchema, f)
		}
	}
}

func TestIncrementalPartialRestartAndHistoricalFacts(t *testing.T) {
	e, root, db := fixtureEngine(t, "codex")
	p := filepath.Join(root, "rollout.jsonl")
	write(t, p, header("one")+codex("task_started", "old", 0)+codex("task_complete", "old", 1))
	scan(t, e)
	if len(all(t, e)) != 3 || len(liveEnds(t, e)) != 0 {
		t.Fatal("historical facts missing or notified")
	}
	appendFile(t, p, []byte(codex("task_started", "new", 2)))
	scan(t, e)
	line := codex("task_complete", "new", 3)
	appendFile(t, p, []byte(line[:len(line)-2]))
	scan(t, e)
	if len(liveEnds(t, e)) != 0 {
		t.Fatal("partial line published")
	}
	appendFile(t, p, []byte(line[len(line)-2:]))
	scan(t, e)
	scan(t, e)
	if len(liveEnds(t, e)) != 1 {
		t.Fatal("completion not exactly once")
	}
	appendFile(t, p, []byte(codex("task_started", "offline", 4)+codex("task_complete", "offline", 5)))
	e.Store.Close()
	s, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	restarted, err := NewEngine(s, e.Roots)
	if err != nil {
		t.Fatal(err)
	}
	scan(t, restarted)
	if len(liveEnds(t, restarted)) != 1 {
		t.Fatal("offline historical completion notified")
	}
	snapshot, _ := s.Snapshot()
	b, _ := json.Marshal(snapshot)
	if bytes.Contains(b, []byte(`"state":"idle"`)) || bytes.Contains(b, []byte(`"confidence"`)) {
		t.Fatal("core inferred Agent state")
	}
	before := len(all(t, restarted))
	scan(t, restarted)
	if len(all(t, restarted)) != before {
		t.Fatal("silence fabricated a state change")
	}
}

func TestNativeOldUnattributedAndFailedEndsRemainFacts(t *testing.T) {
	e, root, _ := fixtureEngine(t, "codex")
	p := filepath.Join(root, "x.jsonl")
	write(t, p, header("one")+codex("task_started", "a", 0))
	scan(t, e)
	appendFile(t, p, []byte(codex("task_started", "b", 1)+codex("task_complete", "a", 2)+codex("task_complete", "", 3)))
	scan(t, e)
	ends := liveEnds(t, e)
	if len(ends) != 2 || ends[0].OperationID != "a" || ends[1].OperationID != "" || ends[1].Data.Outcome != "" || ends[1].NativeReason != "completed" {
		t.Fatal("native boundary metadata lost")
	}
	appendFile(t, p, []byte(`{"type":"event_msg","timestamp":"`+ts(4)+`","payload":{"type":"task_complete","turn_id":"b","error":{"message":"SECRET_CONTENT"}}}`+"\n"))
	scan(t, e)
	ends = liveEnds(t, e)
	if ends[len(ends)-1].Data.Outcome != "failed" {
		t.Fatal("error treated as completion")
	}
	b, _ := json.Marshal(all(t, e))
	if bytes.Contains(b, []byte("SECRET_CONTENT")) {
		t.Fatal("body leaked")
	}
}

func TestNewLiveFileAndInvalidNativeMetadata(t *testing.T) {
	e, root, _ := fixtureEngine(t, "codex")
	scan(t, e)
	p := filepath.Join(root, "short.jsonl")
	write(t, p, header("short")+codex("task_started", "one", 0)+codex("task_complete", "one", 1))
	scan(t, e)
	if len(liveEnds(t, e)) != 1 {
		t.Fatal("short post-start task was hidden as history")
	}
	write(t, filepath.Join(root, "oversized.jsonl"), header(strings.Repeat("x", 257)))
	scan(t, e)
	found := false
	for _, src := range e.sources {
		if src.Reason == "invalid_native_metadata" {
			found = true
			if src.Offset != 0 {
				t.Fatal("invalid metadata advanced checkpoint")
			}
		}
	}
	if !found {
		t.Fatal("invalid native metadata not degraded")
	}
}

func TestClaudePiRetainStopReasonsWithoutInventingRunEnd(t *testing.T) {
	for _, agent := range []string{"claude", "pi"} {
		t.Run(agent, func(t *testing.T) {
			e, root, _ := fixtureEngine(t, agent)
			p := filepath.Join(root, "s.jsonl")
			if agent == "claude" {
				write(t, p, `{"type":"user","sessionId":"one","uuid":"u","message":{"content":"SECRET_PROMPT"}}`+"\n")
			} else {
				write(t, p, `{"type":"session","version":3,"id":"one"}`+"\n"+`{"type":"message","id":"u","parentId":null,"message":{"role":"user","content":"SECRET_PROMPT"}}`+"\n")
			}
			scan(t, e)
			if agent == "claude" {
				appendFile(t, p, []byte(`{"type":"assistant","sessionId":"one","uuid":"a","message":{"role":"assistant","stop_reason":"end_turn","content":[]}}`+"\n"))
			} else {
				appendFile(t, p, []byte(`{"type":"message","id":"a","parentId":"u","message":{"role":"assistant","stopReason":"stop","content":[]}}`+"\n"))
			}
			scan(t, e)
			facts := all(t, e)
			f := facts[len(facts)-1]
			if f.Kind != "message.observed" || f.Data.StopReason == "" || len(liveEnds(t, e)) != 0 {
				t.Fatal("message boundary lost or upgraded")
			}
			b, _ := json.Marshal(facts)
			if bytes.Contains(b, []byte("SECRET_PROMPT")) {
				t.Fatal("prompt leaked")
			}
		})
	}
}

func TestDeepseekFrameBatchSeedApprovalAndRecovery(t *testing.T) {
	e, root, _ := fixtureEngine(t, "deepseek")
	p := filepath.Join(root, "session.v3.jsonl.zstd")
	write(t, p, "")
	appendFile(t, p, frame(t, `{"type":"session","version":3,"id":"one","isSeeded":true}`+"\n"+dsh("turn/start", `{"turn":5}`, 0)+dsh("turn/end", `{"turn":5,"reason":{"kind":"completed"}}`, 1)+dsh("session/end-seed", `{"inherited":true}`, 2)))
	scan(t, e)
	if len(all(t, e)) != 1 {
		t.Fatal("inherited facts attributed to child")
	}
	b := frame(t, dsh("turn/start", `{"turn":6}`, 3)+dsh("tool/call", `{"callId":"c"}`, 4)+dsh("approval/asked", `{"id":"r"}`, 5)+dsh("approval/decided", `{"id":"r","outcome":"allowed-once"}`, 6)+dsh("tool/result", `{"message":{"content":[{"type":"tool-result","toolCallId":"c","isError":true}]}}`, 7)+dsh("turn/end", `{"turn":6,"reason":{"kind":"completed"}}`, 8))
	appendFile(t, p, b[:len(b)-1])
	scan(t, e)
	if len(all(t, e)) != 1 {
		t.Fatal("partial frame committed")
	}
	appendFile(t, p, b[len(b)-1:])
	scan(t, e)
	facts := all(t, e)
	if len(facts) != 7 || facts[2].Kind != "tool.observed" || facts[3].Data.Phase != "requested" || facts[4].Data.Phase != "resolved" || facts[5].Data.Outcome != "failed" || len(liveEnds(t, e)) != 1 {
		t.Fatal("batch facts collapsed", facts)
	}
	appendFile(t, p, frame(t, dsh("turn/end", `{"turn":6,"reason":{"kind":"interrupted"}}`, 9)))
	scan(t, e)
	if len(liveEnds(t, e)) != 1 {
		t.Fatal("recovery ending notified")
	}
}

func TestRollbackRebuildCorruptionAndGenerationIsolation(t *testing.T) {
	e, root, _ := fixtureEngine(t, "codex")
	p := filepath.Join(root, "x.jsonl")
	write(t, p, header("one"))
	scan(t, e)
	var prior int64
	for _, src := range e.sources {
		prior = src.Offset
	}
	_, err := e.Store.db.Exec(`CREATE TRIGGER fail_facts BEFORE INSERT ON facts BEGIN SELECT RAISE(ABORT,'fixture'); END`)
	if err != nil {
		t.Fatal(err)
	}
	appendFile(t, p, []byte(codex("task_started", "new", 0)))
	if e.Scan() == nil {
		t.Fatal("storage failure hidden")
	}
	for _, src := range e.sources {
		if src.Offset != prior {
			t.Fatal("failed commit advanced memory")
		}
	}
	_, _ = e.Store.db.Exec(`DROP TRIGGER fail_facts`)
	scan(t, e)
	write(t, p, header("new")+codex("task_started", "b", 0)+codex("task_complete", "b", 1))
	write(t, filepath.Join(root, "bad.jsonl"), "{broken}\n")
	scan(t, e)
	if len(liveEnds(t, e)) != 0 {
		t.Fatal("replacement history notified")
	}
	snapshot, _ := e.Snapshot()
	if len(snapshot.Sessions) != 1 || snapshot.Sessions[0].SessionID != "new" {
		t.Fatal("replacement left ghost evidence")
	}
	d, dir, _ := fixtureEngine(t, "deepseek")
	write(t, filepath.Join(dir, "session.v3.jsonl"), `{"type":"session","version":3,"id":"d"}`+"\n")
	scan(t, d)
	write(t, filepath.Join(dir, "session.v4.jsonl"), `{"type":"session","version":4,"id":"d"}`+"\n")
	scan(t, d)
	for _, src := range d.sources {
		if src.Generation == 3 && src.Health != "superseded" {
			t.Fatal("old generation not marked")
		}
	}
	write(t, filepath.Join(dir, "session.v4.jsonl.zstd"), string(frame(t, `{"type":"session","version":4,"id":"d"}`+"\n")))
	scan(t, d)
	found := false
	for _, src := range d.sources {
		if src.Reason == "ambiguous_encoding" {
			found = true
		}
	}
	if !found {
		t.Fatal("ambiguous encoding hidden")
	}
}

func TestExternalIdempotencyProvenanceSequenceAndSnapshotBound(t *testing.T) {
	e, _, _ := fixtureEngine(t, "codex")
	f := input("same")
	id, dup, err := e.Store.Ingest(context.Background(), f)
	if err != nil || dup {
		t.Fatal(err)
	}
	second, dup, err := e.Store.Ingest(context.Background(), f)
	if err != nil || !dup || second != id {
		t.Fatal("idempotency failed")
	}
	f.Data.State = "failed"
	if _, _, err = e.Store.Ingest(context.Background(), f); err != ErrConflict {
		t.Fatal("conflict accepted")
	}
	f = input("sequence-1")
	seq := int64(1)
	f.SourceSequence = &seq
	f.StreamID = "stream"
	if _, _, err = e.Store.Ingest(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	f.EventID = "sequence-3"
	seq = 3
	if _, _, err = e.Store.Ingest(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := e.Snapshot()
	if snapshot.Coverage != "partial" {
		t.Fatal("sequence gap hidden")
	}
	facts := all(t, e)
	last := facts[len(facts)-1]
	if last.Origin != "external" || last.ReportedBasis != "native_event" || !last.SequenceGap {
		t.Fatal("external report upgraded or gap lost")
	}
	for n := 0; n < 35; n++ {
		f = input(fmt.Sprint(n))
		f.OperationID = fmt.Sprint(n)
		if _, _, err = e.Store.Ingest(context.Background(), f); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, _ = e.Snapshot()
	if len(snapshot.Sessions[0].Facts) != 32 || snapshot.Coverage != "partial" {
		t.Fatal("operation history unbounded")
	}
}

func TestHTTPScopesValidationAndSSERecovery(t *testing.T) {
	e, root, _ := fixtureEngine(t, "codex")
	write(t, filepath.Join(root, "x.jsonl"), header("one"))
	scan(t, e)
	handler := Handler(e, "READ_SECRET", "WRITE_SECRET")
	for _, test := range []struct {
		method, path, token, body string
		status                    int
	}{{"GET", "/v2/info", "", "", 401}, {"GET", "/v2/info", "WRITE_SECRET", "", 403}, {"POST", "/v2/ingest", "READ_SECRET", "{}", 403}, {"POST", "/v2/ingest", "WRITE_SECRET", "{SECRET_BROKEN", 400}, {"POST", "/v2/ingest", "WRITE_SECRET", strings.Repeat("x", MaxInput+1), 413}, {"GET", "/v1/status", "READ_SECRET", "", 410}} {
		r := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		r.Header.Set("Authorization", "Bearer "+test.token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("%s %s: %d", test.method, test.path, w.Code)
		}
		if strings.Contains(w.Body.String(), "SECRET") {
			t.Fatal("error leaked credential or body")
		}
	}
	r := httptest.NewRequest("GET", "/v2/info", nil)
	r.Header.Set("Authorization", "Bearer READ_SECRET")
	r.Header.Set("Origin", "http://example.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("Origin accepted")
	}
	b, _ := json.Marshal(input("http"))
	for _, want := range []int{201, 200} {
		r = httptest.NewRequest("POST", "/v2/ingest", bytes.NewReader(b))
		r.Header.Set("Authorization", "Bearer WRITE_SECRET")
		r.Header.Set("Content-Type", "application/json")
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	first := func(cursor string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		r, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/v2/events", nil)
		r.Header.Set("Authorization", "Bearer READ_SECRET")
		r.Header.Set("Last-Event-ID", cursor)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		reader := bufio.NewReader(resp.Body)
		kind := ""
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(line, "event: ") {
				kind = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			}
			if line == "\n" {
				return kind
			}
		}
	}
	if first("") != "snapshot" || first(e.Store.Epoch+":0") != "fact" || first("wrong:0") != "reset" {
		t.Fatal("SSE recovery contract")
	}
	facts := make([]Fact, EventLimit+2)
	for i := range facts {
		facts[i] = Fact{InputEvent: input(fmt.Sprint(i)), Origin: "external", SourceID: "retention"}
	}
	if err := e.Store.Commit(nil, facts); err != nil {
		t.Fatal(err)
	}
	retained := all(t, e)
	if len(retained) != EventLimit || first(e.Store.Epoch+":0") != "reset" {
		t.Fatal("retention/reset failed")
	}
}

func TestLeaseFrameBoundsAndWatcher(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), "lease")
	release, err := AcquireLease(leasePath)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := AcquireLease(leasePath); err == nil {
		second()
		t.Fatal("duplicate lease")
	}
	release()
	b := frame(t, "{}\n")
	for n := 0; n < len(b); n++ {
		if _, err := frameSize(b[:n]); err == nil {
			t.Fatal("partial frame accepted")
		}
	}
	if n, err := frameSize(b); err != nil || n != len(b) {
		t.Fatal("complete frame rejected")
	}
	p := filepath.Join(t.TempDir(), "limit.jsonl")
	write(t, p, strings.Repeat("x", maxLine+1))
	f, _ := os.Open(p)
	defer f.Close()
	if _, err := readUnits(f, 0, maxLine+1); err == nil {
		t.Fatal("unbounded line")
	}
	e, root, _ := fixtureEngine(t, "codex")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx, 250*time.Millisecond) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	write(t, filepath.Join(root, "new", "x.jsonl"), header("one"))
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, _ := e.Snapshot()
		if len(snapshot.Sessions) == 1 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("new directory not discovered")
}

func TestRecoveryBoundsIncludeLargeIdentityAndPendingRecords(t *testing.T) {
	for _, largeIdentity := range []bool{false, true} {
		snapshot := Snapshot{Schema: 2, Epoch: "test", Coverage: "complete"}
		for n := 0; n < 4096; n++ {
			f := Fact{InputEvent: input(fmt.Sprint(n)), ID: int64(n + 1), SourceID: fmt.Sprint(n), Origin: "external"}
			ledger := EvidenceSession{Agent: f.Agent, Namespace: f.Namespace, SessionID: f.SessionID, SourceID: f.SourceID, Origin: f.Origin, Facts: []Fact{f}, Coverage: "complete", IncompleteOperations: []string{}}
			if largeIdentity {
				ledger.Agent = strings.Repeat("界", 256)
				ledger.Namespace = ledger.Agent
				ledger.SessionID = ledger.Agent
				for i := 0; i < 33; i++ {
					ledger.IncompleteOperations = append(ledger.IncompleteOperations, strings.Repeat("界", 255)+"x")
				}
			} else {
				ledger.Facts[0].NativeReason = strings.Repeat("界", 256)
				ledger.Facts = append(ledger.Facts, ledger.Facts[0], ledger.Facts[0])
			}
			snapshot.Sessions = append(snapshot.Sessions, ledger)
		}
		boundSnapshot(&snapshot)
		b, err := json.Marshal(snapshot)
		if err != nil || len(b) > MaxSnapshotBytes || snapshot.Coverage != "partial" {
			t.Fatalf("unbounded recovery: bytes=%d err=%v coverage=%s", len(b), err, snapshot.Coverage)
		}
	}
}

func TestExternalDiagnosticsKeepAmbiguousNamesSeparate(t *testing.T) {
	e, _, _ := fixtureEngine(t, "codex")
	for i, pair := range [][2]string{{"ab", "c"}, {"a", "bc"}} {
		f := input(fmt.Sprint(i))
		f.Agent, f.Namespace = pair[0], pair[1]
		if _, _, err := e.Store.Ingest(context.Background(), f); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal(e.Diagnostics())
	var health struct {
		Sources []SourceStatus `json:"sources"`
	}
	if err := json.Unmarshal(b, &health); err != nil || len(health.Sources) != 2 {
		t.Fatalf("external diagnostic identity collision: %s", b)
	}
}
