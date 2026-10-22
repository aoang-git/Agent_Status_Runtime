package monitor

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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
		{"agent": "codex", "surfaces": []string{"cli", "desktop"}, "source": "file", "activity": "partial", "tools": "partial", "human_wait": "unsupported", "turn_end": "explicit_records_only", "desktop_certification": "unverified"},
		{"agent": "claude", "surfaces": []string{"cli", "desktop"}, "source": "file", "activity": "partial", "tools": "partial", "human_wait": "unsupported", "turn_end": "unsupported", "desktop_certification": "unverified"},
		{"agent": "deepseek", "source": "file", "formats": []int{3, 4}, "activity": "partial", "tools": "partial", "human_wait": "inferred_requests_only", "turn_end": "explicit_records_only"},
		{"agent": "pi", "source": "file", "formats": []int{3}, "activity": "partial", "tools": "partial", "human_wait": "unsupported", "turn_end": "unsupported", "no_session": "unsupported"},
	}
}

func Handler(e *Engine, token string) http.Handler {
	mux := http.NewServeMux()
	jsonResponse := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, map[string]any{"version": Version, "schema": 1, "mode": "passive", "epoch": e.Store.Epoch})
	})
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, e.Diagnostics()) })
	mux.HandleFunc("GET /v1/capabilities", func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, Capabilities()) })
	snapshot := func(w http.ResponseWriter, r *http.Request) {
		s, err := e.Snapshot()
		if err != nil {
			http.Error(w, "snapshot_failed", 500)
			return
		}
		jsonResponse(w, s)
	}
	mux.HandleFunc("GET /v1/status", snapshot)
	mux.HandleFunc("GET /v1/sessions", snapshot)
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) { stream(r.Context(), w, r, e) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Origin") != "" {
			http.Error(w, "cross_origin_requests_not_supported", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func stream(ctx context.Context, w http.ResponseWriter, r *http.Request, e *Engine) {
	var after int64
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.URL.Query().Get("after")
	}
	reset := false
	if cursor != "" {
		parts := strings.Split(cursor, ":")
		if len(parts) != 2 {
			http.Error(w, "invalid_cursor", 400)
			return
		}
		var err error
		after, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || after < 0 {
			http.Error(w, "invalid_cursor", 400)
			return
		}
		reset = parts[0] != e.Store.Epoch
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
	if cursor == "" || reset {
		s, err := e.Snapshot()
		if err != nil {
			return
		}
		kind := "snapshot"
		if reset {
			kind = "reset"
		}
		if send(kind, s.Revision, s) != nil {
			return
		}
		after = s.Revision
	}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		events, latest, err := e.Store.Events(after)
		if err != nil {
			return
		}
		if after > latest || (len(events) > 0 && events[0].ID > after+1) {
			s, err := e.Snapshot()
			if err != nil {
				return
			}
			if send("reset", s.Revision, s) != nil {
				return
			}
			after = s.Revision
		} else {
			for _, event := range events {
				if send(event.Type, event.ID, event) != nil {
					return
				}
				after = event.ID
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
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
