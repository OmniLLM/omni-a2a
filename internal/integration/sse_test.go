package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/card"
	"github.com/OmniLLM/omni-agent-hub/internal/config"
	"github.com/OmniLLM/omni-agent-hub/internal/dispatch"
	"github.com/OmniLLM/omni-agent-hub/internal/registry"
	"github.com/OmniLLM/omni-agent-hub/internal/store"
	"github.com/OmniLLM/omni-agent-hub/internal/transport"
)

// bootHubWithHandler is like bootHub but lets the caller install a custom
// upstream handler so tests can drive SSE / arbitrary responses.
func bootHubWithHandler(t *testing.T, upstreamHandler http.Handler) (hub *httptest.Server, cleanup func()) {
	t.Helper()

	// A card is needed so the composite card lists the upstream's skill —
	// wrap the caller's handler with a card responder at the well-known path.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(a2a.AgentCard{
			Name: "fake", URL: "http://fake",
			Capabilities: a2a.AgentCapabilities{Streaming: true},
			Skills:       []a2a.AgentSkill{{ID: "chat", Name: "Chat"}},
		})
	})
	mux.Handle("POST /", upstreamHandler)
	upstream := httptest.NewServer(mux)

	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	cfg := &config.Config{
		Server: config.ServerConfig{
			APIKey:    "client-k",
			AdminKey:  "admin-k",
			PublicURL: "http://hub",
		},
		Hub: config.HubConfig{Name: "IntegHub"},
	}
	reg := registry.New(db, nil)
	if _, err := reg.Add(context.Background(), registry.AddInput{
		Name: "fake", BaseURL: upstream.URL, Auth: config.AuthConfig{Scheme: "none"},
	}); err != nil {
		t.Fatalf("Add upstream: %v", err)
	}
	if err := reg.RefreshAll(context.Background()); err != nil {
		t.Fatalf("RefreshAll: %v", err)
	}
	cb := card.Start(context.Background(), reg, card.FromConfig(cfg, "test"))
	cb.Rebuild()
	disp := dispatch.New(reg, db)
	tsrv := transport.New(transport.Deps{
		Cfg: cfg, Reg: reg, Card: cb, Store: db,
		Unary: disp, Stream: disp, Version: "test",
	})
	hub = httptest.NewServer(tsrv.Handler())
	cleanup = func() {
		hub.Close()
		upstream.Close()
		db.Close()
	}
	return hub, cleanup
}

// sseThreeEvents streams working → working → completed and closes cleanly.
func sseThreeEvents() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		states := []a2a.TaskState{a2a.TaskStateWorking, a2a.TaskStateWorking, a2a.TaskStateCompleted}
		for _, s := range states {
			payload, _ := json.Marshal(a2a.TaskStatusUpdateEvent{
				TaskID: "upstream-abc",
				Status: a2a.TaskStatus{State: s},
				Final:  s.IsTerminal(),
			})
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
	})
}

// sseAbnormalClose emits one non-terminal event and then closes without
// sending a completed/failed/canceled state.
func sseAbnormalClose() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		payload, _ := json.Marshal(a2a.TaskStatusUpdateEvent{
			TaskID: "upstream-abc",
			Status: a2a.TaskStatus{State: a2a.TaskStateWorking},
		})
		fmt.Fprintf(w, "data: %s\n\n", payload)
		flusher.Flush()
		// Hang up early — this is the "clean EOF without terminal" case.
	})
}

// readSSE collects `data:` payloads from resp until EOF or timeout.
func readSSE(t *testing.T, resp *http.Response, want int) []map[string]any {
	t.Helper()
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	var out []map[string]any
	deadline := time.After(5 * time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			var env map[string]any
			if err := json.Unmarshal([]byte(payload), &env); err != nil {
				continue
			}
			out = append(out, env)
			if len(out) >= want {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-deadline:
		t.Fatalf("timed out reading SSE (got %d/%d events)", len(out), want)
	}
	return out
}

func TestIntegration_SSE_ThreeEventsThroughTransport(t *testing.T) {
	hub, cleanup := bootHubWithHandler(t, sseThreeEvents())
	defer cleanup()

	body := `{"jsonrpc":"2.0","id":1,"method":"message/sendSubscribe","params":{"skillId":"fake.chat","message":{"role":"user","parts":[{"text":"hi"}]}}}`
	req, _ := http.NewRequest("POST", hub.URL+"/", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-k")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	events := readSSE(t, resp, 3)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	// Every event's id must have been rewritten (not "upstream-abc").
	for i, e := range events {
		if id, _ := e["id"].(string); id == "upstream-abc" {
			t.Errorf("event %d id was not rewritten: %v", i, id)
		}
	}
	// The final event should carry the completed state.
	last, _ := events[2]["status"].(map[string]any)
	if last["state"] != string(a2a.TaskStateCompleted) {
		t.Errorf("last state = %v, want completed", last["state"])
	}
}

func TestIntegration_SSE_AbnormalCloseSynthesizesFailed(t *testing.T) {
	hub, cleanup := bootHubWithHandler(t, sseAbnormalClose())
	defer cleanup()

	body := `{"jsonrpc":"2.0","id":1,"method":"message/sendSubscribe","params":{"skillId":"fake.chat","message":{"role":"user","parts":[{"text":"hi"}]}}}`
	req, _ := http.NewRequest("POST", hub.URL+"/", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-k")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	events := readSSE(t, resp, 2)
	if len(events) < 2 {
		t.Fatalf("got %d events, want >=2 (1 working + 1 synthesized failed)", len(events))
	}
	last := events[len(events)-1]
	status, _ := last["status"].(map[string]any)
	if status["state"] != string(a2a.TaskStateFailed) {
		t.Fatalf("expected synthesized failed terminal event; got state=%v", status["state"])
	}
	if last["final"] != true {
		t.Errorf("expected final=true on synthesized event; got %v", last["final"])
	}
}
