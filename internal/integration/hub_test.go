// Package integration exercises the hub end-to-end against a fake upstream.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/card"
	"github.com/OmniLLM/omni-agent-hub/internal/config"
	"github.com/OmniLLM/omni-agent-hub/internal/dispatch"
	"github.com/OmniLLM/omni-agent-hub/internal/registry"
	"github.com/OmniLLM/omni-agent-hub/internal/store"
	"github.com/OmniLLM/omni-agent-hub/internal/transport"
)

// fakeUpstream tracks tasks by upstream id and serves multi-turn.
type fakeUpstream struct {
	step map[string]int
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{step: map[string]int{}}
}

func (f *fakeUpstream) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(a2a.AgentCard{
			Name: "fake", URL: r.Host,
			Skills: []a2a.AgentSkill{{ID: "chat", Name: "Chat"}},
			Capabilities: a2a.AgentCapabilities{Streaming: false},
		})
	})
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req a2a.JSONRPCRequest
		_ = json.Unmarshal(body, &req)

		switch req.Method {
		case "message/send":
			var params a2a.SendMessageParams
			_ = json.Unmarshal(req.Params, &params)
			// Multi-turn: first call returns input-required with a fixed task id,
			// second call with the same context returns completed.
			taskID := "upstream-task-" + params.ContextID
			f.step[params.ContextID]++
			state := a2a.TaskStateInputRequired
			if f.step[params.ContextID] >= 2 {
				state = a2a.TaskStateCompleted
			}
			writeResp(w, req.ID, a2a.Task{
				TaskID: taskID, ContextID: params.ContextID,
				Status: a2a.TaskStatus{State: state},
			})
		case "tasks/get":
			var p a2a.GetTaskParams
			_ = json.Unmarshal(req.Params, &p)
			writeResp(w, req.ID, a2a.Task{
				TaskID: p.TaskID,
				Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
			})
		}
	})
	return mux
}

func writeResp(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a2a.JSONRPCResponse{JSONRPC: "2.0", ID: id, Result: result})
}

// bootHub returns a running httptest.Server hosting the hub, with a fake upstream registered.
func bootHub(t *testing.T) (hub *httptest.Server, upstreamURL string, cleanup func()) {
	t.Helper()

	fake := newFakeUpstream()
	upstream := httptest.NewServer(fake.Handler())

	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(dbPath)
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
	_, err = reg.Add(context.Background(), registry.AddInput{
		Name: "fake", BaseURL: upstream.URL, Auth: config.AuthConfig{Scheme: "none"},
	})
	if err != nil {
		t.Fatalf("Add upstream: %v", err)
	}
	// Trigger card fetch so composite card includes the skill.
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
	return hub, upstream.URL, cleanup
}

func TestIntegration_CompositeCardExposesUpstreamSkill(t *testing.T) {
	hub, _, cleanup := bootHub(t)
	defer cleanup()
	resp, err := http.Get(hub.URL + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("GET card: %v", err)
	}
	defer resp.Body.Close()
	var c a2a.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(c.Skills) != 1 || c.Skills[0].ID != "fake.chat" {
		t.Fatalf("expected 1 skill fake.chat, got %+v", c.Skills)
	}
}

func TestIntegration_MultiTurn_ContextSticks(t *testing.T) {
	hub, _, cleanup := bootHub(t)
	defer cleanup()

	// Turn 1: send with a specific contextId.
	body := `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"contextId":"ctx-42","skillId":"fake.chat","message":{"role":"user","parts":[{"text":"hi"}]}}}`
	turn1 := postJSON(t, hub, body)
	if turn1.Error != nil {
		t.Fatalf("turn1 error: %+v", turn1.Error)
	}
	task1, _ := turn1.Result.(map[string]any)
	if task1["status"].(map[string]any)["state"] != string(a2a.TaskStateInputRequired) {
		t.Fatalf("expected input-required first, got %+v", task1["status"])
	}
	hubTaskID1 := task1["id"].(string)
	if hubTaskID1 == "upstream-task-ctx-42" {
		t.Fatalf("hub did not rewrite task id")
	}

	// Turn 2: send follow-up with same contextId. Must land on same upstream
	// AND produce the same hub-mapped task id... actually the *hub* mints a new
	// one, but the upstream must be the same (fake will see step=2 for same ctx).
	body2 := `{"jsonrpc":"2.0","id":2,"method":"message/send","params":{"contextId":"ctx-42","skillId":"fake.chat","message":{"role":"user","parts":[{"text":"reply"}]}}}`
	turn2 := postJSON(t, hub, body2)
	if turn2.Error != nil {
		t.Fatalf("turn2 error: %+v", turn2.Error)
	}
	task2, _ := turn2.Result.(map[string]any)
	if task2["status"].(map[string]any)["state"] != string(a2a.TaskStateCompleted) {
		t.Fatalf("expected completed on turn 2, got %+v", task2["status"])
	}
}

type jrpcResult struct {
	Result any               `json:"result"`
	Error  *a2a.JSONRPCError `json:"error"`
}

func postJSON(t *testing.T, hub *httptest.Server, body string) jrpcResult {
	t.Helper()
	req, _ := http.NewRequest("POST", hub.URL+"/", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var out jrpcResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}
