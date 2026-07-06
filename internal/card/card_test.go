package card

import (
	"testing"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/registry"
	"github.com/OmniLLM/omni-agent-hub/internal/store"
)

func TestBuild_NamespacesSkills(t *testing.T) {
	upstreams := []registry.Upstream{
		{
			Name: "hermes", Status: store.StatusHealthy,
			Card: &a2a.AgentCard{
				Name: "hermes", URL: "http://h",
				Capabilities: a2a.AgentCapabilities{Streaming: true},
				Skills: []a2a.AgentSkill{
					{ID: "coding", Name: "Coding"},
					{ID: "chat", Name: "General Chat"},
				},
			},
		},
		{
			Name: "research", Status: store.StatusHealthy,
			Card: &a2a.AgentCard{
				Name: "research", URL: "http://r",
				Capabilities: a2a.AgentCapabilities{PushNotifications: true},
				Skills:       []a2a.AgentSkill{{ID: "search", Name: "Search"}},
			},
		},
	}
	c := build(upstreams, HubIdentity{Name: "Hub", PublicURL: "http://hub"})
	if len(c.Skills) != 3 {
		t.Fatalf("skills count = %d, want 3", len(c.Skills))
	}
	// Namespacing invariant.
	wantIDs := map[string]bool{"hermes.coding": true, "hermes.chat": true, "research.search": true}
	for _, s := range c.Skills {
		if !wantIDs[s.ID] {
			t.Errorf("unexpected skill id: %q", s.ID)
		}
	}
	// Capabilities are the union.
	if !c.Capabilities.Streaming || !c.Capabilities.PushNotifications {
		t.Errorf("capabilities union wrong: %+v", c.Capabilities)
	}
}

func TestBuild_UnhealthyOrNilCardExcluded(t *testing.T) {
	upstreams := []registry.Upstream{
		{Name: "u1", Status: store.StatusUnhealthy, Card: &a2a.AgentCard{
			Name: "u1", URL: "http://u1", Skills: []a2a.AgentSkill{{ID: "x"}},
		}},
		{Name: "u2", Status: store.StatusHealthy, Card: nil},
		{Name: "u3", Status: store.StatusUnknown, Card: &a2a.AgentCard{
			Name: "u3", URL: "http://u3", Skills: []a2a.AgentSkill{{ID: "y"}},
		}},
	}
	c := build(upstreams, HubIdentity{Name: "Hub", PublicURL: "http://hub"})
	if len(c.Skills) != 0 {
		t.Fatalf("expected 0 skills, got %d", len(c.Skills))
	}
}

func TestBuild_AuthWhenAPIKey(t *testing.T) {
	c := build(nil, HubIdentity{Name: "Hub", PublicURL: "http://x", HasAPIKey: true})
	if c.Authentication == nil || len(c.Authentication.Schemes) != 1 || c.Authentication.Schemes[0] != "bearer" {
		t.Errorf("expected bearer auth, got %+v", c.Authentication)
	}
	c2 := build(nil, HubIdentity{Name: "Hub", PublicURL: "http://x", HasAPIKey: false})
	if c2.Authentication != nil {
		t.Errorf("expected nil auth, got %+v", c2.Authentication)
	}
}
