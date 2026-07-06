package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/registry"
	"github.com/OmniLLM/omni-agent-hub/internal/store"
)

// preparedSend holds everything the unary and stream flows need after common
// setup but before actually contacting the upstream.
type preparedSend struct {
	Upstream  registry.Upstream
	HubTaskID string
	ContextID string
	Body      []byte // marshaled JSON-RPC envelope
}

// prepareSend performs the shared pre-flight for message/send and
// message/sendSubscribe:
//   - resolve upstream from the router resolution
//   - check the breaker; return ErrUnavailable if open
//   - mint or reuse hub_task_id for multi-turn stickiness
//   - persist a placeholder task row for new tasks
//   - rewrite message text (if @mention stripped) and skill id
//   - marshal the outbound JSON-RPC envelope
//
// method is either "message/send" or "message/sendSubscribe" and dictates the
// value written into the outbound JSON-RPC envelope's `method` field. Every
// audit-log entry uses store.EventSend for both.
func (d *Dispatcher) prepareSend(ctx context.Context, req UnaryRequest, method string) (*preparedSend, error) {
	upstream, err := d.upstreamFor(req.Res.UpstreamID)
	if err != nil {
		return nil, err
	}
	if !d.Reg.CanAttempt(upstream.ID) {
		_ = d.Store.WriteAudit(ctx, store.AuditEntry{
			TraceID: req.TraceID, UpstreamID: string(upstream.ID),
			Event: store.EventBreakerBlocked,
		})
		return nil, a2a.NewError(a2a.ErrUnavailable, "Upstream unavailable",
			fmt.Sprintf("upstream %q is unhealthy (breaker open)", upstream.Name))
	}

	// hub_task_id: reuse an existing non-terminal one for the same
	// (contextID, upstreamID) so multi-turn conversations show up as one task
	// AND we don't violate UNIQUE(upstream_id, upstream_task_id) when the
	// upstream returns its same task id again.
	hubTaskID := uuid.NewString()
	contextID := req.ContextID
	if contextID == "" {
		contextID = uuid.NewString()
	} else if existing, ok := d.Store.LookupHubTaskByContext(ctx, contextID, string(upstream.ID)); ok {
		hubTaskID = existing
	}
	// CreateTask only when this is a brand-new task row.
	if _, err := d.Store.GetTask(ctx, hubTaskID); errors.Is(err, store.ErrNotFound) {
		if err := d.Store.CreateTask(ctx, hubTaskID, contextID, string(upstream.ID)); err != nil {
			return nil, fmt.Errorf("dispatch: create task row: %w", err)
		}
	}
	_ = d.Store.WriteAudit(ctx, store.AuditEntry{
		TraceID: req.TraceID, HubTaskID: hubTaskID,
		UpstreamID: string(upstream.ID), Event: store.EventSend,
		Detail: map[string]string{"reason": string(req.Res.Reason), "method": method},
	})

	// Rewrite message: if the router stripped an @mention, use the rewritten
	// text in the first part.
	msg := req.Message
	if req.Res.RewrittenText != "" && len(msg.Parts) > 0 {
		msg.Parts = append([]a2a.Part(nil), msg.Parts...)
		msg.Parts[0].Text = req.Res.RewrittenText
	}
	params, err := json.Marshal(a2a.SendMessageParams{
		Message: msg, ContextID: contextID, SkillID: req.Res.UpstreamSkillID,
	})
	if err != nil {
		return nil, fmt.Errorf("dispatch: marshal params: %w", err)
	}
	body, err := json.Marshal(a2a.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(fmt.Sprintf("%q", hubTaskID)),
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, fmt.Errorf("dispatch: marshal request: %w", err)
	}
	slog.Debug("dispatch prepared",
		"trace_id", req.TraceID,
		"method", method,
		"upstream", upstream.Name,
		"upstream_id", upstream.ID,
		"hub_task_id", hubTaskID,
		"context_id", contextID,
		"upstream_skill_id", req.Res.UpstreamSkillID,
	)
	return &preparedSend{
		Upstream: upstream, HubTaskID: hubTaskID, ContextID: contextID, Body: body,
	}, nil
}
