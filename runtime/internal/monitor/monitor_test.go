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
	s := NewStore()
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
	return e.Store.Recent().Facts
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
	return InputEvent{Schema: 3, ProducerID: "adapter", EventID: id, Agent: "new-agent", Namespace: "local", SessionID: "s", OperationID: "op", Kind: "state.reported", ReportedBasis: "native_event", Data: EventData{State: "completed"}}
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
				_, err := DecodeReport(sample.Event)
				if (err == nil) != group.valid {
					t.Fatalf("receiver: %v", err)
				}
				if err = schema.Validate(v); (err == nil) != group.valid {
					t.Fatalf("schema: %v", err)
				}
			})
		}
	}
	for _, bad := range []string{`{"schema":3,"schema":1}`, `{"Schema":2}`, `[]`, `{} {}`, `{"kind":null}`} {
		if _, err := DecodeInput([]byte(bad)); err == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
	if _, err := DecodeInput([]byte(strings.Repeat("x", MaxInput+1))); err == nil {
		t.Fatal("unbounded body")
	}
}

func TestPublishedOutputConformsToSchema(t *testing.T) {
	c := jsonschema.NewCompiler()
	c.AssertFormat = true
	if err := c.AddResource("schema.json", bytes.NewReader(SchemaJSON())); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile("schema.json#/$defs/recent")
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore()
	_, _, _ = s.Ingest(context.Background(), input("external"))
	b, _ := json.Marshal(s.Recent())
	var v any
	_ = json.Unmarshal(b, &v)
	if err = schema.Validate(v); err != nil {
		t.Fatal(err)
	}
}

func TestIncrementalPartialRestartAndBaseline(t *testing.T) {
	e, root, _ := fixtureEngine(t, "codex")
	p := filepath.Join(root, "x.jsonl")
	write(t, p, header("one")+codex("task_started", "old", 0)+codex("task_complete", "old", 1))
	scan(t, e)
	if len(all(t, e)) != 0 {
		t.Fatal("startup history broadcast")
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
		t.Fatal("end not exactly once")
	}
	restarted, err := NewEngine(NewStore(), e.Roots)
	if err != nil {
		t.Fatal(err)
	}
	scan(t, restarted)
	if len(all(t, restarted)) != 0 || restarted.Store.Epoch == e.Store.Epoch {
		t.Fatal("restart restored history")
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
	if len(all(t, e)) != 0 {
		t.Fatal("inherited facts attributed to child")
	}
	b := frame(t, dsh("turn/start", `{"turn":6}`, 3)+dsh("tool/call", `{"callId":"c"}`, 4)+dsh("approval/asked", `{"id":"r"}`, 5)+dsh("approval/decided", `{"id":"r","outcome":"allowed-once"}`, 6)+dsh("tool/result", `{"message":{"content":[{"type":"tool-result","toolCallId":"c","isError":true}]}}`, 7)+dsh("turn/end", `{"turn":6,"reason":{"kind":"completed"}}`, 8))
	appendFile(t, p, b[:len(b)-1])
	scan(t, e)
	if len(all(t, e)) != 0 {
		t.Fatal("partial frame committed")
	}
	appendFile(t, p, b[len(b)-1:])
	scan(t, e)
	facts := all(t, e)
	if len(facts) != 6 || facts[1].Kind != "tool.observed" || facts[2].Data.Phase != "requested" || facts[3].Data.Phase != "resolved" || facts[4].Data.Outcome != "failed" || len(liveEnds(t, e)) != 1 {
		t.Fatal("batch facts collapsed", facts)
	}
	appendFile(t, p, frame(t, dsh("turn/end", `{"turn":6,"reason":{"kind":"interrupted"}}`, 9)))
	scan(t, e)
	if len(liveEnds(t, e)) != 1 {
		t.Fatal("recovery ending notified")
	}
}

func TestRebuildCorruptionAndGenerationIsolation(t *testing.T) {
	e, root, _ := fixtureEngine(t, "codex")
	p := filepath.Join(root, "x.jsonl")
	write(t, p, header("one"))
	scan(t, e)
	write(t, p, header("new")+codex("task_started", "b", 0)+codex("task_complete", "b", 1))
	write(t, filepath.Join(root, "bad.jsonl"), "{broken}\n")
	scan(t, e)
	if len(liveEnds(t, e)) != 0 {
		t.Fatal("replacement history notified")
	}
	d, dir, _ := fixtureEngine(t, "deepseek")
	write(t, filepath.Join(dir, "session.v3.jsonl"), "{\"type\":\"session\",\"version\":3,\"id\":\"d\"}\n")
	scan(t, d)
	write(t, filepath.Join(dir, "session.v4.jsonl"), "{\"type\":\"session\",\"version\":4,\"id\":\"d\"}\n")
	scan(t, d)
	for _, src := range d.sources {
		if src.Generation == 3 && src.Health != "superseded" {
			t.Fatal("old generation not marked")
		}
	}
	write(t, filepath.Join(dir, "session.v4.jsonl.zstd"), string(frame(t, "{\"type\":\"session\",\"version\":4,\"id\":\"d\"}\n")))
	scan(t, d)
	found := false
	for _, src := range d.sources {
		if src.Reason == "ambiguous_encoding" {
			found = true
		}
	}
	if !found {
		t.Fatal("ambiguity hidden")
	}
}

func TestExternalIdempotencyProvenanceAndBounds(t *testing.T) {
	s := NewStore()
	clock := time.Now()
	s.now = func() time.Time { return clock }
	f := input("same")
	id, dup, err := s.Ingest(context.Background(), f)
	if err != nil || dup {
		t.Fatal(err)
	}
	second, dup, err := s.Ingest(context.Background(), f)
	if err != nil || !dup || second != id {
		t.Fatal("dedup")
	}
	f.Data.State = "failed"
	if _, _, err = s.Ingest(context.Background(), f); err != ErrConflict {
		t.Fatal("conflict")
	}
	seq := int64(1)
	f = input("seq1")
	f.StreamID = "stream"
	f.SourceSequence = &seq
	_, _, _ = s.Ingest(context.Background(), f)
	seq = 3
	f.EventID = "seq3"
	_, _, _ = s.Ingest(context.Background(), f)
	recent := s.Recent()
	last := recent.Facts[len(recent.Facts)-1]
	if !last.SequenceGap || last.Origin != "external" || last.ReportedBasis != "native_event" {
		t.Fatal("provenance")
	}
	batch := make([]Fact, EventLimit+2)
	for i := range batch {
		batch[i] = Fact{InputEvent: input(fmt.Sprint(i)), Origin: "file", SourceID: "file"}
	}
	_ = s.Commit(nil, batch)
	events, _, gap, _ := s.Read(0)
	if !gap || len(events) != 0 || len(s.Recent().Facts) != EventLimit {
		t.Fatal("count bound")
	}
	clock = clock.Add(11 * time.Minute)
	if len(s.Recent().Facts) != 0 || len(s.dedup) != 0 {
		t.Fatal("age bound")
	}
	_, _, _ = s.Ingest(context.Background(), input("same"))
	if len(s.Recent().Facts) != 1 {
		t.Fatal("expired dedup persisted")
	}
}

func TestHTTPScopesAndLiveOnlySSE(t *testing.T) {
	e, _, _ := fixtureEngine(t, "codex")
	handler := Handler(e, "READ_SECRET", "WRITE_SECRET")
	report := `{"schema":3,"producer_id":"p","event_id":"1","agent":"pi","session_id":"s","operation_id":"r","state":"working","reported_basis":"native_event"}`
	for _, test := range []struct {
		method, path, token, body string
		status                    int
	}{
		{"GET", "/v3/info", "", "", 401}, {"GET", "/v3/info", "WRITE_SECRET", "", 403}, {"POST", "/v3/ingest", "READ_SECRET", "{}", 403},
		{"POST", "/v3/ingest", "WRITE_SECRET", "{SECRET", 400}, {"POST", "/v3/ingest", "WRITE_SECRET", strings.Repeat("x", MaxInput+1), 413},
		{"GET", "/v2/info", "READ_SECRET", "", 404}, {"POST", "/v3/ingest", "WRITE_SECRET", report, 201}, {"POST", "/v3/ingest", "WRITE_SECRET", report, 200},
		{"GET", "/v3/events?after=1", "READ_SECRET", "", 400},
	} {
		r := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		r.Header.Set("Authorization", "Bearer "+test.token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("%s: %d %s", test.path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "SECRET") {
			t.Fatal("secret leaked")
		}
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/v3/events", nil)
	r.Header.Set("Authorization", "Bearer READ_SECRET")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	next := func() string {
		var frame strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			frame.WriteString(line)
			if line == "\n" {
				return frame.String()
			}
		}
	}
	if first := next(); !strings.Contains(first, "event: ready") || strings.Contains(first, "state.reported") {
		t.Fatal(first)
	}
	_, _, _ = e.Store.Ingest(ctx, input("after-ready"))
	if second := next(); !strings.Contains(second, "event: fact") || !strings.Contains(second, "after-ready") {
		t.Fatal(second)
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
		health := e.Diagnostics().(Health)
		if len(health.Sources) == 1 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("new directory not discovered")
}

func TestByteBoundsWakeAndCancellation(t *testing.T) {
	s := NewStore()
	_, _, _, wake := s.Read(0)
	f := Fact{InputEvent: input("large"), Origin: "external", SourceID: "large"}
	f.NativeReason = strings.Repeat("x", 1<<20)
	for i := 0; i < 12; i++ {
		_ = s.Commit(nil, []Fact{f})
	}
	select {
	case <-wake:
	default:
		t.Fatal("subscriber not woken")
	}
	if s.bytes > MaxRecentBytes || s.dropped == 0 {
		t.Fatal("byte cap")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.Ingest(ctx, input("cancel")); err == nil {
		t.Fatal("cancel ignored")
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

func TestRebuildThenAppendAndDelayedNativeTimeRemainLive(t *testing.T) {
	e, root, _ := fixtureEngine(t, "codex")
	p := filepath.Join(root, "x.jsonl")
	write(t, p, header("original"))
	scan(t, e)
	write(t, p, header("replacement")+codex("task_started", "old", 0))
	scan(t, e)
	appendFile(t, p, []byte("{\"type\":\"event_msg\",\"timestamp\":\"2000-01-01T00:00:00Z\",\"payload\":{\"type\":\"task_started\",\"turn_id\":\"live\"}}\n"))
	scan(t, e)
	facts := all(t, e)
	if len(facts) != 1 || facts[0].OperationID != "live" {
		t.Fatal("new append was rebaselined or filtered", facts)
	}
}
func TestMissingSourceExpiresAndStoredInputsAreImmutable(t *testing.T) {
	e, root, _ := fixtureEngine(t, "codex")
	p := filepath.Join(root, "x.jsonl")
	write(t, p, header("one"))
	scan(t, e)
	_ = os.Remove(p)
	for _, src := range e.sources {
		src.LastSeen = time.Now().Add(-2 * time.Hour)
	}
	_ = e.poll(nil)
	scan(t, e)
	if len(e.sources) != 0 {
		t.Fatal("missing source kept alive")
	}
	s := NewStore()
	seq := int64(1)
	f := input("immutable")
	f.StreamID = "s"
	f.SourceSequence = &seq
	_, _, _ = s.Ingest(context.Background(), f)
	seq = 99
	if *s.Recent().Facts[0].SourceSequence != 1 {
		t.Fatal("stored observation mutated")
	}
}

func TestConcurrentHealthReadsAndStreamExpiry(t *testing.T) {
	e, _, _ := fixtureEngine(t, "codex")
	_, _, _ = e.Store.Ingest(context.Background(), input("health"))
	done := make(chan bool, 20)
	for i := 0; i < 20; i++ {
		go func() {
			for n := 0; n < 100; n++ {
				h := e.Diagnostics().(Health)
				if len(h.Sources) != 1 {
					done <- false
					return
				}
			}
			done <- true
		}()
	}
	for i := 0; i < 20; i++ {
		if !<-done {
			t.Fatal("concurrent diagnostics changed sources")
		}
	}
	clock := time.Now()
	s := NewStore()
	s.now = func() time.Time { return clock }
	seq := int64(1)
	f := input("1")
	f.StreamID = "stream"
	f.SourceSequence = &seq
	_, _, _ = s.Ingest(context.Background(), f)
	clock = clock.Add(61 * time.Minute)
	_ = s.Stats()
	if len(s.sequences) != 0 {
		t.Fatal("stream TTL")
	}
}
func TestDiagnosticLogRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	l, err := NewDiagnosticLog(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = l.Write([]byte(strings.Repeat("x", 1<<20)))
	_, _ = l.Write([]byte("next"))
	_ = l.Close()
	for _, name := range []string{path, path + ".1"} {
		info, err := os.Stat(name)
		if err != nil || info.Size() > 1<<20 {
			t.Fatal("unbounded log", err)
		}
	}
}

func TestRecreatedDirectoryRestoresNotificationsWithoutPolling(t *testing.T) {
	e, root, _ := fixtureEngine(t, "codex")
	dir := filepath.Join(root, "nested")
	p := filepath.Join(dir, "x.jsonl")
	write(t, p, header("old"))
	scan(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx, time.Hour) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	wait := func(predicate func() bool) {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if predicate() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("watcher recovery timeout")
	}
	initial := time.Now()
	wait(func() bool { return e.Diagnostics().(Health).LastScanAt.After(initial) })
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	write(t, p, header("replacement"))
	recreated := time.Now()
	wait(func() bool { return e.Diagnostics().(Health).LastScanAt.After(recreated) })
	appendFile(t, p, []byte(codex("task_started", "new", 1)))
	wait(func() bool {
		for _, f := range all(t, e) {
			if f.OperationID == "new" {
				return true
			}
		}
		return false
	})
}
