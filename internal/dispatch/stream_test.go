package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/registry"
	"github.com/OmniLLM/omni-agent-hub/internal/router"
)

// sseHandler emits three status updates then closes.
type sseHandler struct {
	upstreamTaskID string
	states         []a2a.TaskState
	abruptClose    bool
}

func (h *sseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		panic("no flusher")
	}
	for i, state := range h.states {
		payload := a2a.TaskStatusUpdateEvent{
			TaskID: h.upstreamTaskID,
			Status: a2a.TaskStatus{State: state},
			Final:  state.IsTerminal(),
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
		if h.abruptClose && i == len(h.states)-1 {
			// Simulate an abnormal close by closing the connection early.
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				conn.Close()
			}
			return
		}
	}
}

func TestSendMessageSubscribe_ThreeEventsAllRewritten(t *testing.T) {
	handler := &sseHandler{
		upstreamTaskID: "upstream-abc",
		states: []a2a.TaskState{
			a2a.TaskStateWorking,
			a2a.TaskStateWorking,
			a2a.TaskStateCompleted,
		},
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	d, u := setupWithUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r) // reuse same handler at the upstream URL
	}))
	// Rewrite base URL to point at srv (setupWithUpstream registered a
	// different httptest server). Simpler: build a new one that only speaks
	// SSE by re-adding.
	_ = u

	// Just rebuild: register a fresh upstream at srv.URL.
	reg := d.Reg
	fresh, err := reg.Add(context.Background(), registry.AddInput{
		Name: "sse-fake", BaseURL: srv.URL,
	})
	if err != nil {
		// The name may already exist from setupWithUpstream — but we used
		// "fake" there, so this should succeed.
		t.Fatalf("Add sse-fake: %v", err)
	}
	reg.RecordSuccess(fresh.ID)

	ch, err := d.SendMessageSubscribe(context.Background(), UnaryRequest{
		Res:     router.Resolution{UpstreamID: fresh.ID, Reason: router.ReasonSkill},
		Message: a2a.Message{Role: a2a.RoleUser, Parts: []a2a.Part{{Text: "hi"}}},
	})
	if err != nil {
		t.Fatalf("SendMessageSubscribe: %v", err)
	}
	var events []StreamEvent
	for e := range ch {
		events = append(events, e)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	// Every event's id must be rewritten (not "upstream-abc").
	for i, e := range events {
		var env map[string]any
		if err := json.Unmarshal(e.Data, &env); err != nil {
			t.Fatalf("event %d parse: %v", i, err)
		}
		if id, _ := env["id"].(string); id == "upstream-abc" {
			t.Errorf("event %d: id was NOT rewritten", i)
		}
	}
	if !events[len(events)-1].Final {
		t.Errorf("last event should be final")
	}
}

// Needed for the import to be used.

