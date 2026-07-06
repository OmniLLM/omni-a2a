package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// AuditEvent enumerates the dispatch events written to audit_log.
type AuditEvent string

const (
	EventSend           AuditEvent = "send"
	EventForward        AuditEvent = "forward"
	EventResponse       AuditEvent = "resp"
	EventError          AuditEvent = "error"
	EventCancel         AuditEvent = "cancel"
	EventBreakerOpen    AuditEvent = "breaker-open"
	EventBreakerBlocked AuditEvent = "breaker-blocked"
	EventBreakerClose   AuditEvent = "breaker-close"
	EventStreamStart    AuditEvent = "stream-start"
	EventStreamEnd      AuditEvent = "stream-end"
	EventCardRefresh    AuditEvent = "card-refresh"
)

// AuditEntry is a row to write to audit_log.
type AuditEntry struct {
	TraceID    string
	HubTaskID  string
	UpstreamID string
	Event      AuditEvent
	Detail     any
}

// WriteAudit appends an event to audit_log. Failures are returned but callers
// often ignore them (audit logging must not block dispatch).
func (s *Store) WriteAudit(ctx context.Context, e AuditEntry) error {
	var detail string
	if e.Detail != nil {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			return fmt.Errorf("marshal audit detail: %w", err)
		}
		detail = string(b)
	}
	_, err := s.db.ExecContext(s.withCtx(ctx),
		`INSERT INTO audit_log (ts, trace_id, hub_task_id, upstream_id, event, detail_json)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		nowUTC(), e.TraceID, e.HubTaskID, e.UpstreamID, string(e.Event), detail,
	)
	if err != nil {
		return fmt.Errorf("write audit: %w", err)
	}
	return nil
}

// VacuumAudit trims audit_log to the newest max rows. Called on startup.
func (s *Store) VacuumAudit(ctx context.Context, max int) error {
	if max <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(s.withCtx(ctx),
		`DELETE FROM audit_log WHERE id NOT IN (
		    SELECT id FROM audit_log ORDER BY id DESC LIMIT ?
		)`, max)
	if err != nil {
		return fmt.Errorf("vacuum audit: %w", err)
	}
	return nil
}
