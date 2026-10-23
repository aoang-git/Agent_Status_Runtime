package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-status-runtime/internal/monitor"
)

func cliInput(t *testing.T, body string, args ...string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	in, err := os.Create(filepath.Join(dir, "in"))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	_, _ = in.WriteString(body)
	_, _ = in.Seek(0, 0)
	out, err := os.Create(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = in, out
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	err = run(args)
	b, _ := os.ReadFile(out.Name())
	return string(b), err
}

const reportExample = `{"schema":3,"producer_id":"adapter","event_id":"stable","agent":"custom","session_id":"s","operation_id":"op","state":"completed","reported_basis":"self_report"}`

func TestReportOfflineValidationAndPromptDoNotStartService(t *testing.T) {
	dir := t.TempDir()
	if _, err := cliInput(t, reportExample, "report", "--validate-only", "--data-dir", dir); err != nil {
		t.Fatal(err)
	}
	if output, err := cliInput(t, reportExample, "report", "--best-effort", "--data-dir", dir); err != nil || output != "" {
		t.Fatal("best effort leaked stdout or failed", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "private")); !os.IsNotExist(err) {
		t.Fatal("report created service state")
	}
	prompt := monitor.IntegrationPrompt("EXE_PATH", dir)
	for _, required := range []string{"EXE_PATH", dir, "report --data-dir", "reported_basis", "self_report", "JSON Schema"} {
		if !strings.Contains(prompt, required) {
			t.Fatal("incomplete prompt", required)
		}
	}
	if strings.Contains(prompt, "{{") {
		t.Fatal("unexpanded prompt")
	}
}

func TestReportRereadsRotatedCredentialAndRetriesSameEvent(t *testing.T) {
	dir := t.TempDir()
	private := filepath.Join(dir, "private")
	_ = os.MkdirAll(private, 0700)
	requests := 0
	var server *httptest.Server
	writeConnection := func(token string) {
		b, _ := json.Marshal(connection{URL: server.URL, Token: token})
		if err := os.WriteFile(filepath.Join(private, "ingest.json"), b, 0600); err != nil {
			t.Error(err)
		}
	}
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var event monitor.InputEvent
		if json.NewDecoder(r.Body).Decode(&event) != nil || event.EventID != "stable" {
			t.Error("event changed on retry")
		}
		if requests == 1 {
			writeConnection("new")
			w.WriteHeader(401)
			return
		}
		if r.URL.Path != "/v3/ingest" || r.Header.Get("Authorization") != "Bearer new" {
			t.Error("wrong write credential")
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"schema":3,"id":1,"epoch":"test","duplicate":false}`))
	}))
	defer server.Close()
	writeConnection("old")
	output, err := cliInput(t, reportExample, "report", "--data-dir", dir)
	if err != nil || requests != 2 || !strings.Contains(output, `"id":1`) {
		t.Fatal(requests, output, err)
	}
	output, err = cliInput(t, reportExample, "report", "--best-effort", "--data-dir", dir)
	if err != nil || output != "" {
		t.Fatal("best effort printed ACK")
	}
}

func TestReportHasOneSecondBudgetAndRejectsNonLoopbackConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body); <-r.Context().Done() }))
	defer server.Close()
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "private"), 0700)
	path := filepath.Join(dir, "private", "ingest.json")
	b, _ := json.Marshal(connection{URL: server.URL, Token: "token"})
	_ = os.WriteFile(path, b, 0600)
	start := time.Now()
	_, err := cliInput(t, reportExample, "report", "--data-dir", dir)
	if err == nil || time.Since(start) > 1500*time.Millisecond {
		t.Fatal("report exceeded bounded send budget", err)
	}
	b, _ = json.Marshal(connection{URL: "http://example.com", Token: "SECRET"})
	_ = os.WriteFile(path, b, 0600)
	if _, err = loadConnection(dir, "ingest.json"); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("unsafe connection")
	}
}
