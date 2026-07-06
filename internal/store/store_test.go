package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpen_MigratesToV1(t *testing.T) {
	s := openTestStore(t)
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if v != currentSchemaVersion {
		t.Fatalf("user_version = %d, want %d", v, currentSchemaVersion)
	}
	// Reopening the same file must not re-run migrations (idempotent).
	_ = s.Close()
	s2, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	s2.Close()
}

func TestUpsertUpstream_RoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	in := UpstreamRow{
		ID: "u-1", Name: "hermes", BaseURL: "http://x", AuthScheme: "bearer",
		AuthToken: "t", Prefix: "@hermes", Enabled: true,
		Source: SourceConfig, Status: StatusUnknown,
	}
	if err := s.UpsertUpstream(ctx, in); err != nil {
		t.Fatalf("UpsertUpstream: %v", err)
	}
	out, err := s.GetUpstreamByName(ctx, "hermes")
	if err != nil {
		t.Fatalf("GetUpstreamByName: %v", err)
	}
	if out.BaseURL != "http://x" || out.AuthToken != "t" || !out.Enabled {
		t.Errorf("round-trip mismatch: %+v", out)
	}
	// Config→Admin: admin source should stick even if we later re-upsert as config.
	adminRow := in
	adminRow.Source = SourceAdmin
	if err := s.UpsertUpstream(ctx, adminRow); err != nil {
		t.Fatalf("upsert as admin: %v", err)
	}
	// Now re-upsert as config; source should stay admin.
	if err := s.UpsertUpstream(ctx, in); err != nil {
		t.Fatalf("re-upsert as config: %v", err)
	}
	after, _ := s.GetUpstreamByName(ctx, "hermes")
	if after.Source != SourceAdmin {
		t.Errorf("expected admin source to stick, got %s", after.Source)
	}
}

func TestTasks_MapAndLookup(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Upstream row first (task FK'd to it)
	_ = s.UpsertUpstream(ctx, UpstreamRow{
		ID: "u-1", Name: "hermes", BaseURL: "http://x",
		AuthScheme: "bearer", Source: SourceConfig, Status: StatusHealthy,
	})

	if err := s.CreateTask(ctx, "hub-t1", "ctx-1", "u-1"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := s.MapTaskID(ctx, "hub-t1", "u-1", "up-task-a"); err != nil {
		t.Fatalf("MapTaskID: %v", err)
	}
	got, err := s.LookupUpstreamTaskID(ctx, "hub-t1")
	if err != nil || got != "up-task-a" {
		t.Fatalf("LookupUpstreamTaskID = %q err=%v", got, err)
	}

	// Sticky context lookup returns the upstream while task is non-terminal.
	up, ok := s.LookupContext(ctx, "ctx-1")
	if !ok || up != "u-1" {
		t.Fatalf("LookupContext(ctx-1) = (%q,%v)", up, ok)
	}
	// After marking terminal, stickiness should drop.
	_ = s.UpdateTaskSnapshot(ctx, "hub-t1", a2a.TaskStateCompleted, &a2a.Task{TaskID: "hub-t1"})
	_, ok = s.LookupContext(ctx, "ctx-1")
	if ok {
		t.Fatalf("LookupContext should be empty after terminal state")
	}
}

func TestAudit_WriteAndVacuum(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < 25; i++ {
		if err := s.WriteAudit(ctx, AuditEntry{
			TraceID: "tr", Event: EventSend, Detail: map[string]int{"i": i},
		}); err != nil {
			t.Fatalf("WriteAudit: %v", err)
		}
	}
	if err := s.VacuumAudit(ctx, 10); err != nil {
		t.Fatalf("VacuumAudit: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 10 {
		t.Errorf("count after vacuum = %d, want 10", n)
	}
}
