// Package router resolves an incoming A2A request to a specific upstream by a
// deterministic set of rules. It is pure: no I/O, no locks, no side effects —
// the caller assembles a Snapshot from a registry.List() result and any
// context stickiness they've looked up in the store.
//
// Resolution order (first match wins):
//
//  1. Context stickiness — if req.ContextID is present and snap.StickyUpstream
//     returns an ID, resolve to that upstream, regardless of skill/text.
//     This keeps multi-turn tasks (input-required → follow-up) glued to their
//     original upstream.
//
//  2. Skill match — if req.SkillID is present:
//       - namespaced (`upstream.skill`) → look up prefix in snap
//       - un-namespaced → resolve only if unambiguous in the snapshot's skill
//         index
//
//  3. @mention — if req.Text begins with `@name` or `@name `, strip and route.
//
//  4. Prefix — for each upstream with a non-empty Prefix, `HasPrefix(Text, prefix)`
//     matches (no rewrite).
package router

import (
	"regexp"
	"strings"

	"github.com/OmniLLM/omni-agent-hub/internal/registry"
)

// Reason enumerates why the resolver picked an upstream.
type Reason string

const (
	ReasonContext Reason = "context"
	ReasonSkill   Reason = "skill"
	ReasonMention Reason = "mention"
	ReasonPrefix  Reason = "prefix"
)

// Request is what the router resolves.
type Request struct {
	SkillID   string
	Text      string
	ContextID string
}

// Resolution describes the chosen upstream and any rewrites the router
// performed on the outbound request.
type Resolution struct {
	UpstreamID      registry.UpstreamID
	UpstreamSkillID string // empty if no skill rewrite needed
	RewrittenText   string // empty if no @mention rewrite performed
	Reason          Reason
}

// Snapshot is an immutable view assembled by the caller from registry.List()
// and (optionally) a store.LookupContext hit.
type Snapshot interface {
	Healthy() []registry.Upstream
	// All includes unhealthy upstreams — needed for context stickiness to
	// deliberately route to an unhealthy upstream (so dispatch can return
	// "unavailable" rather than silently switching upstreams mid-conversation).
	All() []registry.Upstream
	ByName(name string) (registry.Upstream, bool)
	StickyUpstream(contextID string) (registry.UpstreamID, bool)
}

// namespaced is `name.skill` — greedy at the first dot so skill IDs may
// themselves contain dots.
var namespacedRE = regexp.MustCompile(`^([^.]+)\.(.+)$`)

// mentionRE matches a leading `@name` or `@name<space>...` optionally followed
// by whitespace.
var mentionRE = regexp.MustCompile(`^@([A-Za-z0-9_.-]+)(?:\s+(.*))?$`)

// Resolve returns the resolution and true if any rule matched.
func Resolve(req Request, snap Snapshot) (Resolution, bool) {
	// 1. Context stickiness — highest priority.
	if req.ContextID != "" {
		if id, ok := snap.StickyUpstream(req.ContextID); ok {
			return Resolution{
				UpstreamID: id,
				Reason:     ReasonContext,
			}, true
		}
	}

	// 2. Skill routing.
	if req.SkillID != "" {
		if m := namespacedRE.FindStringSubmatch(req.SkillID); m != nil {
			name, skill := m[1], m[2]
			if u, ok := snap.ByName(name); ok && u.Enabled {
				return Resolution{
					UpstreamID:      u.ID,
					UpstreamSkillID: skill,
					Reason:          ReasonSkill,
				}, true
			}
		}
		// Un-namespaced skill: only accept an unambiguous match.
		if id, ok := lookupUnambiguousSkill(snap.Healthy(), req.SkillID); ok {
			return Resolution{
				UpstreamID:      id,
				UpstreamSkillID: req.SkillID,
				Reason:          ReasonSkill,
			}, true
		}
	}

	// 3. @mention.
	text := req.Text
	if strings.HasPrefix(text, "@") {
		if m := mentionRE.FindStringSubmatch(text); m != nil {
			name, rest := m[1], m[2]
			if u, ok := snap.ByName(name); ok && u.Enabled && u.Status != "unhealthy" {
				return Resolution{
					UpstreamID:    u.ID,
					RewrittenText: rest,
					Reason:        ReasonMention,
				}, true
			}
		}
	}

	// 4. Prefix.
	for _, u := range snap.Healthy() {
		if u.Prefix != "" && strings.HasPrefix(text, u.Prefix) {
			return Resolution{
				UpstreamID: u.ID,
				Reason:     ReasonPrefix,
			}, true
		}
	}

	return Resolution{}, false
}

// lookupUnambiguousSkill scans all healthy upstreams and returns an
// UpstreamID iff exactly one upstream advertises the given un-namespaced skill.
func lookupUnambiguousSkill(healthy []registry.Upstream, skillID string) (registry.UpstreamID, bool) {
	var found registry.UpstreamID
	hits := 0
	for _, u := range healthy {
		if u.Card == nil {
			continue
		}
		for _, s := range u.Card.Skills {
			if s.ID == skillID {
				found = u.ID
				hits++
				break
			}
		}
	}
	if hits == 1 {
		return found, true
	}
	return "", false
}

// StaticSnapshot is a plain-struct Snapshot for callers that don't want to
// implement the interface themselves. It exposes the pieces to fill in and
// implements the four methods.
type StaticSnapshot struct {
	Upstreams []registry.Upstream
	Sticky    map[string]registry.UpstreamID
}

// NewSnapshot constructs a StaticSnapshot from a slice.
func NewSnapshot(ups []registry.Upstream) *StaticSnapshot {
	return &StaticSnapshot{Upstreams: ups, Sticky: map[string]registry.UpstreamID{}}
}

// WithSticky sets stickiness for the given contextId.
func (s *StaticSnapshot) WithSticky(contextID string, id registry.UpstreamID) *StaticSnapshot {
	if s.Sticky == nil {
		s.Sticky = map[string]registry.UpstreamID{}
	}
	s.Sticky[contextID] = id
	return s
}

// Healthy implements Snapshot.
func (s *StaticSnapshot) Healthy() []registry.Upstream {
	out := s.Upstreams[:0:0]
	for _, u := range s.Upstreams {
		if u.Enabled && u.Status == "healthy" {
			out = append(out, u)
		}
	}
	return out
}

// All implements Snapshot.
func (s *StaticSnapshot) All() []registry.Upstream { return s.Upstreams }

// ByName implements Snapshot.
func (s *StaticSnapshot) ByName(name string) (registry.Upstream, bool) {
	for _, u := range s.Upstreams {
		if u.Name == name {
			return u, true
		}
	}
	return registry.Upstream{}, false
}

// StickyUpstream implements Snapshot.
func (s *StaticSnapshot) StickyUpstream(contextID string) (registry.UpstreamID, bool) {
	id, ok := s.Sticky[contextID]
	return id, ok
}
