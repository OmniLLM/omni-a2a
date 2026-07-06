package transport

import (
	"context"
	"fmt"
	"net/http"

	"github.com/OmniLLM/omni-agent-hub/internal/store"
)

// handleMetrics writes Prometheus text-format metrics. Kept dependency-free
// (no prometheus client library) — the metric set is small enough to inline.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	all := s.deps.Reg.List()

	fmt.Fprintf(w, "# HELP omni_a2a_upstream_healthy 1 if upstream is healthy.\n")
	fmt.Fprintf(w, "# TYPE omni_a2a_upstream_healthy gauge\n")
	fmt.Fprintf(w, "# HELP omni_a2a_upstream_consecutive_failures consecutive failure count.\n")
	fmt.Fprintf(w, "# TYPE omni_a2a_upstream_consecutive_failures gauge\n")
	for _, u := range all {
		h := 0
		if u.Status == store.StatusHealthy {
			h = 1
		}
		fmt.Fprintf(w, "omni_a2a_upstream_healthy{upstream=%q} %d\n", u.Name, h)
		fmt.Fprintf(w, "omni_a2a_upstream_consecutive_failures{upstream=%q} %d\n", u.Name, u.ConsecutiveFailures)
	}

	n, _ := s.deps.Store.CountActiveTasks(context.WithoutCancel(r.Context()))
	fmt.Fprintf(w, "# HELP omni_a2a_tasks_active count of non-terminal tasks.\n")
	fmt.Fprintf(w, "# TYPE omni_a2a_tasks_active gauge\n")
	fmt.Fprintf(w, "omni_a2a_tasks_active %d\n", n)
}
