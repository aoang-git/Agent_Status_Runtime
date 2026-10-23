package monitor

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"
)

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func NewToken() string { return randomToken() }

func Capabilities() any {
	return []map[string]any{
		{"agent": "codex", "surfaces": []string{"cli", "desktop"}, "source": "file", "facts": []string{"session.observed", "input.observed", "message.observed", "tool.observed", "lifecycle.observed"}, "limitations": []string{"desktop_unverified", "missing_turn_id_not_attributed"}},
		{"agent": "claude", "surfaces": []string{"cli", "desktop"}, "source": "file", "facts": []string{"session.observed", "input.observed", "message.observed", "tool.observed"}, "limitations": []string{"message_end_is_not_run_end", "desktop_unverified"}},
		{"agent": "deepseek", "source": "file", "formats": []int{3, 4}, "facts": []string{"session.observed", "message.observed", "tool.observed", "approval.observed", "lifecycle.observed"}, "limitations": []string{"approval_request_is_not_human_wait", "recovery_end_is_historical"}},
		{"agent": "pi", "source": "file", "formats": []int{3}, "facts": []string{"session.observed", "input.observed", "message.observed", "tool.observed"}, "limitations": []string{"message_end_is_not_agent_settled", "in_memory_branches_not_observable"}},
	}
}
func apiError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"schema": 3, "error": code})
}
func Handler(e *Engine, readToken, writeToken string) http.Handler {
	mux := http.NewServeMux()
	respond := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /v3/info", func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string]any{"version": Version, "schema": 3, "mode": "facts", "epoch": e.Store.Epoch, "event_limit": EventLimit, "max_input_bytes": MaxInput, "max_recent_bytes": MaxRecentBytes, "retention_seconds": 600, "idempotency_limit": DedupLimit})
	})
	mux.HandleFunc("GET /v3/schema", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/schema+json")
		_, _ = w.Write(SchemaJSON())
	})
	mux.HandleFunc("GET /v3/health", func(w http.ResponseWriter, r *http.Request) { respond(w, e.Diagnostics()) })
	mux.HandleFunc("GET /v3/capabilities", func(w http.ResponseWriter, r *http.Request) { respond(w, Capabilities()) })
	mux.HandleFunc("GET /v3/recent", func(w http.ResponseWriter, r *http.Request) { respond(w, e.Store.Recent()) })
	mux.HandleFunc("GET /v3/events", func(w http.ResponseWriter, r *http.Request) { stream(r.Context(), w, r, e) })
	mux.HandleFunc("POST /v3/ingest", func(w http.ResponseWriter, r *http.Request) {
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			apiError(w, 415, "json_required")
			return
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(2 * time.Second))
		b, err := io.ReadAll(io.LimitReader(r.Body, MaxInput+1))
		if err != nil {
			apiError(w, 400, "body_read_failed")
			return
		}
		if len(b) > MaxInput {
			apiError(w, 413, "body_too_large")
			return
		}
		input, err := DecodeReport(b)
		if err != nil {
			apiError(w, 400, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		id, duplicate, err := e.Store.Ingest(ctx, input)
		if errors.Is(err, ErrConflict) {
			apiError(w, 409, "event_id_conflict")
			return
		}
		if err != nil {
			apiError(w, 503, "storage_failed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if !duplicate {
			w.WriteHeader(201)
		}
		respond(w, map[string]any{"schema": 3, "id": id, "epoch": e.Store.Epoch, "duplicate": duplicate})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Header.Get("Origin") != "" {
			apiError(w, 403, "cross_origin_requests_not_supported")
			return
		}
		auth := r.Header.Get("Authorization")
		isRead := readToken != "" && subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+readToken)) == 1
		isWrite := writeToken != "" && subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+writeToken)) == 1
		if !isRead && !isWrite {
			apiError(w, 401, "unauthorized")
			return
		}
		if (r.URL.Path == "/v3/ingest" && !isWrite) || (r.URL.Path != "/v3/ingest" && !isRead) {
			apiError(w, 403, "insufficient_scope")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func stream(ctx context.Context, w http.ResponseWriter, r *http.Request, e *Engine) {
	if r.Header.Get("Last-Event-ID") != "" || r.URL.RawQuery != "" {
		apiError(w, 400, "history_not_supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	controller := http.NewResponseController(w)
	send := func(kind string, id int64, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err = fmt.Fprintf(w, "id: %s:%d\nevent: %s\ndata: %s\n\n", e.Store.Epoch, id, kind, b); err != nil {
			return err
		}
		return controller.Flush()
	}
	// Captured before ready is sent: facts accepted during the write remain live.
	_, after, _, _ := e.Store.Read(-1)
	boundary := func(id int64) any { return map[string]any{"schema": 3, "epoch": e.Store.Epoch, "revision": id} }
	if send("ready", after, boundary(after)) != nil {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		events, latest, gap, changed := e.Store.Read(after)
		if gap {
			if send("gap", latest, boundary(latest)) != nil {
				return
			}
			after = latest
		} else {
			for _, f := range events {
				if send("fact", f.ID, f) != nil {
					return
				}
				after = f.ID
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-heartbeat.C:
			_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			if controller.Flush() != nil {
				return
			}
		}
	}
}
