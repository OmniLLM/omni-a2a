// Package card owns the composite AgentCard advertised at
// /.well-known/agent-card.json. A single builder goroutine subscribes to
// registry events and swaps the current card via an atomic.Pointer, so
// handlers read the card lock-free.
package card

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/config"
	"github.com/OmniLLM/omni-agent-hub/internal/registry"
	"github.com/OmniLLM/omni-agent-hub/internal/store"
)

// debounceWindow coalesces bursts of registry events into a single rebuild.
const debounceWindow = 100 * time.Millisecond

// Builder exposes the current composite AgentCard.
type Builder interface {
	Current() a2a.AgentCard
	Rebuild()
}

// HubIdentity is the subset of config needed to build the card. Kept small so
// callers don't have to pass a whole *config.Config.
type HubIdentity struct {
	Name        string
	Description string
	PublicURL   string
	Version     string
	HasAPIKey   bool
}

// FromConfig extracts a HubIdentity from a config.
func FromConfig(cfg *config.Config, buildVersion string) HubIdentity {
	return HubIdentity{
		Name:        cfg.Hub.Name,
		Description: cfg.Hub.Description,
		PublicURL:   cfg.Server.PublicURL,
		Version:     buildVersion,
		HasAPIKey:   cfg.Server.APIKey != "",
	}
}

// builderImpl is the concrete implementation.
type builderImpl struct {
	current  atomic.Pointer[a2a.AgentCard]
	reg      registry.Registry
	identity HubIdentity
}

// Start creates a Builder and spawns its background goroutine. The goroutine
// exits when ctx is Done.
func Start(ctx context.Context, reg registry.Registry, id HubIdentity) Builder {
	b := &builderImpl{reg: reg, identity: id}
	b.Rebuild() // seed
	go b.loop(ctx)
	return b
}

func (b *builderImpl) Current() a2a.AgentCard {
	p := b.current.Load()
	if p == nil {
		return a2a.AgentCard{}
	}
	return *p
}

func (b *builderImpl) Rebuild() {
	card := build(b.reg.List(), b.identity)
	b.current.Store(&card)
}

// loop consumes registry events and rebuilds the card, debouncing bursts
// within debounceWindow.
func (b *builderImpl) loop(ctx context.Context) {
	events := b.reg.Events()
	var pending bool
	timer := time.NewTimer(time.Hour)
	timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			slog.Debug("card: registry event", "kind", ev.Kind.String(), "id", ev.ID)
			if !pending {
				pending = true
				timer.Reset(debounceWindow)
			}
		case <-timer.C:
			b.Rebuild()
			pending = false
		}
	}
}

// build assembles the composite card from a snapshot of registry state.
// Only healthy upstreams with a non-nil card contribute their skills.
func build(all []registry.Upstream, id HubIdentity) a2a.AgentCard {
	var skills []a2a.AgentSkill
	var anyStream, anyPush bool

	for _, u := range all {
		if u.Status != store.StatusHealthy || u.Card == nil {
			continue
		}
		for _, s := range u.Card.Skills {
			caps := u.Card.Capabilities
			skills = append(skills, a2a.AgentSkill{
				ID:           fmt.Sprintf("%s.%s", u.Name, s.ID),
				Name:         s.Name,
				Description:  s.Description,
				Capabilities: &caps,
			})
		}
		if u.Card.Capabilities.Streaming {
			anyStream = true
		}
		if u.Card.Capabilities.PushNotifications {
			anyPush = true
		}
	}

	var auth *a2a.AgentAuth
	if id.HasAPIKey {
		auth = &a2a.AgentAuth{Schemes: []string{"bearer"}}
	}
	return a2a.AgentCard{
		Name:               id.Name,
		Description:        id.Description,
		URL:                id.PublicURL,
		Version:            id.Version,
		Capabilities:       a2a.AgentCapabilities{Streaming: anyStream, PushNotifications: anyPush},
		Authentication:     auth,
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             skills,
	}
}
