package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/card"
	"github.com/OmniLLM/omni-agent-hub/internal/config"
	"github.com/OmniLLM/omni-agent-hub/internal/dispatch"
	"github.com/OmniLLM/omni-agent-hub/internal/registry"
	"github.com/OmniLLM/omni-agent-hub/internal/store"
	"github.com/OmniLLM/omni-agent-hub/internal/transport"
)

// bootHubEmpty starts a hub with zero upstreams and returns the hub + a
// running fake upstream (not yet registered).
func bootHubEmpty(t *testing.T) (hub *httptest.Server, fakeUpstream *httptest.Server, cleanup func()) {
	t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/agent-card.json" {
			_ = json.NewEncoder(w).Encode(a2a.AgentCard{
				Name: "later-fake", URL: "http://x",
				Skills: []a2a.AgentSkill{{ID: "chat", Name: "Chat"}},
			})
			return
		}
		http.NotFound(w, r)
	}))
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	cfg := &config.Config{
		Server: config.ServerConfig{
			APIKey: "client-k", AdminKey: "admin-k", PublicURL: "http://hub",
		},
		Hub: config.HubConfig{Name: "AdminHub"},
	}
	reg := registry.New(db, nil)
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
		fake.Close()
		db.Close()
	}
	return hub, fake, cleanup
}

// adminReq builds a request signed with the admin key.
func adminReq(method, url, body string) *http.Request {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, r)
	req.Header.Set("Authorization", "Bearer admin-k")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestIntegration_AdminLifecycle_AddRemoveRefresh(t *testing.T) {
	hub, fake, cleanup := bootHubEmpty(t)
	defer cleanup()

	// 1. list — should be empty.
	resp, err := http.DefaultClient.Do(adminReq("GET", hub.URL+"/admin/upstreams", ""))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var list []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 0 {
		t.Fatalf("empty registry expected, got %+v", list)
	}

	// 2. add via admin API.
	addBody := `{"name":"newone","base_url":"` + fake.URL + `","auth":{"scheme":"none"}}`
	resp, err = http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams", addBody))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("add status = %d body=%s", resp.StatusCode, string(body))
	}
	var created map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("no id in create response: %+v", created)
	}

	// 3. duplicate add — must fail with 409.
	resp, _ = http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams", addBody))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate add: got %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()

	// 4. refresh single — should hit the fake's card endpoint.
	resp, err = http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams/"+id+"/refresh", ""))
	if err != nil {
		t.Fatalf("refresh one: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 5. list — should show one upstream, with the card populated.
	resp, _ = http.DefaultClient.Do(adminReq("GET", hub.URL+"/admin/upstreams", ""))
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 1 {
		t.Fatalf("after add expected 1 upstream, got %d", len(list))
	}
	if list[0]["has_card"] != true {
		t.Errorf("expected has_card=true after refresh, got %+v", list[0])
	}

	// 6. admin skills — should include one entry.
	resp, _ = http.DefaultClient.Do(adminReq("GET", hub.URL+"/admin/skills", ""))
	var skills []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&skills)
	resp.Body.Close()
	if len(skills) != 1 || skills[0]["skill_id"] != "newone.chat" {
		t.Fatalf("skills = %+v", skills)
	}

	// 7. delete.
	resp, err = http.DefaultClient.Do(adminReq("DELETE", hub.URL+"/admin/upstreams/"+id, ""))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 8. list — back to empty.
	resp, _ = http.DefaultClient.Do(adminReq("GET", hub.URL+"/admin/upstreams", ""))
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 0 {
		t.Fatalf("after delete expected 0 upstreams, got %d", len(list))
	}
}

func TestIntegration_MetricsEndpoint(t *testing.T) {
	hub, _, cleanup := bootHubEmpty(t)
	defer cleanup()

	// Add one upstream so metrics has something to print.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(a2a.AgentCard{Name: "m", URL: "http://x"})
	}))
	defer fake.Close()
	addBody := `{"name":"m","base_url":"` + fake.URL + `","auth":{"scheme":"none"}}`
	req := adminReq("POST", hub.URL+"/admin/upstreams", addBody)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// Now hit /metrics (public, no auth).
	resp, err := http.Get(hub.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := string(body)
	if !bytes.Contains(body, []byte("omni_a2a_upstream_healthy")) {
		t.Errorf("metrics missing omni_a2a_upstream_healthy:\n%s", got)
	}
	if !bytes.Contains(body, []byte("omni_a2a_tasks_active")) {
		t.Errorf("metrics missing omni_a2a_tasks_active:\n%s", got)
	}
}
