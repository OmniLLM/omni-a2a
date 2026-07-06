package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/OmniLLM/omni-agent-hub/internal/config"
	"github.com/OmniLLM/omni-agent-hub/internal/registry"
)

// upstreamInfoJSON is the shape returned by /admin/upstreams (list + create).
type upstreamInfoJSON struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	BaseURL  string      `json:"base_url"`
	Prefix   string      `json:"prefix,omitempty"`
	Enabled  bool        `json:"enabled"`
	Source   string      `json:"source"`
	Status   string      `json:"status"`
	HasCard  bool        `json:"has_card"`
	Skills   int         `json:"skills"`
}

func upstreamToInfo(u registry.Upstream) upstreamInfoJSON {
	info := upstreamInfoJSON{
		ID:      string(u.ID),
		Name:    u.Name,
		BaseURL: u.BaseURL,
		Prefix:  u.Prefix,
		Enabled: u.Enabled,
		Source:  string(u.Source),
		Status:  string(u.Status),
	}
	if u.Card != nil {
		info.HasCard = true
		info.Skills = len(u.Card.Skills)
	}
	return info
}

func (s *Server) handleAdminListUpstreams(w http.ResponseWriter, _ *http.Request) {
	list := s.deps.Reg.List()
	out := make([]upstreamInfoJSON, 0, len(list))
	for _, u := range list {
		out = append(out, upstreamToInfo(u))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAdminAddUpstream(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string             `json:"name"`
		BaseURL string             `json:"base_url"`
		Prefix  string             `json:"prefix,omitempty"`
		Auth    config.AuthConfig  `json:"auth"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if body.Auth.Scheme == "" {
		body.Auth.Scheme = "none"
	}
	up, err := s.deps.Reg.Add(r.Context(), registry.AddInput{
		Name: body.Name, BaseURL: body.BaseURL, Prefix: body.Prefix, Auth: body.Auth,
	})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, registry.ErrDuplicateName) {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, upstreamToInfo(up))
}

func (s *Server) handleAdminRemoveUpstream(w http.ResponseWriter, r *http.Request) {
	id := registry.UpstreamID(r.PathValue("id"))
	if err := s.deps.Reg.Remove(r.Context(), id); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, registry.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminRefreshOne(w http.ResponseWriter, r *http.Request) {
	id := registry.UpstreamID(r.PathValue("id"))
	// Use a detached context so the refresh (and its DB writes) complete
	// even if the HTTP client disconnects or times out.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	if err := s.deps.Reg.RefreshCard(ctx, id); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	u, _ := s.deps.Reg.Get(id)
	if u.Card == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "refreshed"})
		return
	}
	writeJSON(w, http.StatusOK, u.Card)
}

func (s *Server) handleAdminRefreshAll(w http.ResponseWriter, r *http.Request) {
	// Use a detached context so the refresh (and its DB writes) complete
	// even if the HTTP client disconnects or times out.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
	defer cancel()
	_ = s.deps.Reg.RefreshAll(ctx)
	writeJSON(w, http.StatusOK, map[string]string{"status": "refreshed"})
}

func (s *Server) handleAdminSkills(w http.ResponseWriter, _ *http.Request) {
	type row struct {
		SkillID  string `json:"skill_id"`
		Upstream string `json:"upstream"`
	}
	var out []row
	for _, u := range s.deps.Reg.List() {
		if u.Card == nil {
			continue
		}
		for _, sk := range u.Card.Skills {
			out = append(out, row{SkillID: u.Name + "." + sk.ID, Upstream: u.Name})
		}
	}
	writeJSON(w, http.StatusOK, out)
}
