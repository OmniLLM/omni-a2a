package router

import (
	"testing"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/registry"
	"github.com/OmniLLM/omni-agent-hub/internal/store"
)

func mkUp(name string, prefix string, skills ...string) registry.Upstream {
	sk := make([]a2a.AgentSkill, len(skills))
	for i, s := range skills {
		sk[i] = a2a.AgentSkill{ID: s, Name: s}
	}
	return registry.Upstream{
		ID: registry.UpstreamID("id-" + name), Name: name,
		Prefix: prefix, Enabled: true, Status: store.StatusHealthy,
		Card: &a2a.AgentCard{Name: name, URL: "http://" + name, Skills: sk},
	}
}

func TestResolve_Context(t *testing.T) {
	snap := NewSnapshot([]registry.Upstream{mkUp("hermes", "", "coding")}).
		WithSticky("ctx-1", "id-hermes")
	got, ok := Resolve(Request{ContextID: "ctx-1", SkillID: "other.thing"}, snap)
	if !ok || got.Reason != ReasonContext || got.UpstreamID != "id-hermes" {
		t.Fatalf("expected context stickiness, got %+v ok=%v", got, ok)
	}
}

func TestResolve_NamespacedSkill(t *testing.T) {
	snap := NewSnapshot([]registry.Upstream{mkUp("hermes", "", "coding")})
	got, ok := Resolve(Request{SkillID: "hermes.coding"}, snap)
	if !ok || got.Reason != ReasonSkill || got.UpstreamSkillID != "coding" {
		t.Fatalf("bad namespaced resolution: %+v ok=%v", got, ok)
	}
}

func TestResolve_UnnamespacedUnambiguous(t *testing.T) {
	snap := NewSnapshot([]registry.Upstream{
		mkUp("hermes", "", "coding"),
		mkUp("research", "", "search"),
	})
	got, ok := Resolve(Request{SkillID: "coding"}, snap)
	if !ok || got.UpstreamID != "id-hermes" {
		t.Fatalf("expected hermes for unambiguous 'coding', got %+v ok=%v", got, ok)
	}
}

func TestResolve_UnnamespacedAmbiguousRejected(t *testing.T) {
	snap := NewSnapshot([]registry.Upstream{
		mkUp("a", "", "shared"),
		mkUp("b", "", "shared"),
	})
	_, ok := Resolve(Request{SkillID: "shared"}, snap)
	if ok {
		t.Fatalf("expected no resolution for ambiguous skill")
	}
}

func TestResolve_Mention(t *testing.T) {
	snap := NewSnapshot([]registry.Upstream{mkUp("hermes", "", "coding")})
	got, ok := Resolve(Request{Text: "@hermes write me a function"}, snap)
	if !ok || got.Reason != ReasonMention || got.RewrittenText != "write me a function" {
		t.Fatalf("mention resolution wrong: %+v ok=%v", got, ok)
	}
	// Bare mention without body strips to empty text.
	got, ok = Resolve(Request{Text: "@hermes"}, snap)
	if !ok || got.RewrittenText != "" {
		t.Fatalf("bare mention: %+v ok=%v", got, ok)
	}
}

func TestResolve_Prefix(t *testing.T) {
	snap := NewSnapshot([]registry.Upstream{mkUp("omni", "@omnilauncher", "x")})
	got, ok := Resolve(Request{Text: "@omnilauncher please"}, snap)
	if !ok || got.Reason != ReasonPrefix {
		t.Fatalf("prefix resolution wrong: %+v ok=%v", got, ok)
	}
}

func TestResolve_NoMatch(t *testing.T) {
	snap := NewSnapshot([]registry.Upstream{mkUp("hermes", "", "coding")})
	if _, ok := Resolve(Request{Text: "hello"}, snap); ok {
		t.Fatalf("expected no match for plain text")
	}
}

func TestResolve_ContextBeatsEverythingElse(t *testing.T) {
	snap := NewSnapshot([]registry.Upstream{
		mkUp("hermes", "", "coding"),
		mkUp("research", "", "search"),
	}).WithSticky("ctx-1", "id-research")
	got, ok := Resolve(Request{
		ContextID: "ctx-1", SkillID: "hermes.coding", Text: "@hermes hi",
	}, snap)
	if !ok || got.UpstreamID != "id-research" || got.Reason != ReasonContext {
		t.Fatalf("context should win over skill and mention: %+v", got)
	}
}

func TestResolve_UnhealthyMentionSkipped(t *testing.T) {
	u := mkUp("hermes", "", "coding")
	u.Status = store.StatusUnhealthy
	snap := NewSnapshot([]registry.Upstream{u})
	_, ok := Resolve(Request{Text: "@hermes hello"}, snap)
	if ok {
		t.Fatalf("unhealthy @mention should not match (dispatch handles context stickiness only)")
	}
}
