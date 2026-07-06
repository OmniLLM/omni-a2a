package registry

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/config"
	"github.com/OmniLLM/omni-agent-hub/internal/store"
)

// fakeFetcher lets tests control card-fetch results and count calls.
type fakeFetcher struct {
	card *a2a.AgentCard
	err  error
	n    int
}

func (f *fakeFetcher) Fetch(ctx context.Context, baseURL, scheme, token string) (*a2a.AgentCard, error) {
	f.n++
	if f.err != nil {
		return nil, f.err
	}
	return f.card, nil
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestBootstrap_MergesConfigWithDB(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "hermes", URL: "http://x"}}
	r := New(db, f)

	cfg := []config.UpstreamCfg{
		{Name: "hermes", BaseURL: "http://h", Auth: config.AuthConfig{Scheme: "bearer", Token: "t"}, Enabled: true},
		{Name: "research", BaseURL: "http://r", Auth: config.AuthConfig{Scheme: "none"}, Enabled: true},
	}
	if err := Bootstrap(ctx, r, db, cfg); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	list := r.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 upstreams, got %d", len(list))
	}
	// Re-bootstrap with the same cfg → should be idempotent.
	if err := Bootstrap(ctx, r, db, cfg); err != nil {
		t.Fatalf("re-Bootstrap: %v", err)
	}
	if len(r.List()) != 2 {
		t.Fatalf("re-bootstrap changed count: %d", len(r.List()))
	}
}

func TestAdd_DuplicateNameRejected(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "x", URL: "http://x"}}
	r := New(db, f)

	if _, err := r.Add(ctx, AddInput{Name: "a", BaseURL: "http://a"}); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	if _, err := r.Add(ctx, AddInput{Name: "a", BaseURL: "http://a2"}); !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("expected ErrDuplicateName, got %v", err)
	}
}

func TestBreaker_ThreeFailuresFlipsUnhealthy(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "x", URL: "http://x"}}
	r := New(db, f)
	u, err := r.Add(ctx, AddInput{Name: "u1", BaseURL: "http://u"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Drain the initial add/card events so we can assert the health event later.
	drain(r.Events())

	for i := 0; i < 2; i++ {
		r.RecordFailure(u.ID, errors.New("boom"))
	}
	got, _ := r.Get(u.ID)
	if got.Status != store.StatusHealthy && got.Status != store.StatusUnknown {
		t.Fatalf("after 2 failures should still be healthy/unknown, got %s", got.Status)
	}
	r.RecordFailure(u.ID, errors.New("boom"))
	got, _ = r.Get(u.ID)
	if got.Status != store.StatusUnhealthy {
		t.Fatalf("after 3 failures should be unhealthy, got %s", got.Status)
	}

	// Success flips back and emits a health event.
	r.RecordSuccess(u.ID)
	got, _ = r.Get(u.ID)
	if got.Status != store.StatusHealthy || got.ConsecutiveFailures != 0 {
		t.Fatalf("after success: status=%s failures=%d", got.Status, got.ConsecutiveFailures)
	}
}

func TestCanAttempt_Backoff(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	r := New(db, &fakeFetcher{card: &a2a.AgentCard{Name: "x", URL: "http://x"}})
	u, _ := r.Add(ctx, AddInput{Name: "u", BaseURL: "http://u"})

	// Healthy always attempts.
	if !r.CanAttempt(u.ID) {
		t.Fatalf("healthy upstream should attempt")
	}
	// Force unhealthy state.
	for i := 0; i < 3; i++ {
		r.RecordFailure(u.ID, errors.New("x"))
	}
	if r.CanAttempt(u.ID) {
		t.Fatalf("just-failed upstream should not attempt within backoff window")
	}
	// Manually rewind LastFailureAt to 5s ago (backoff at 3 failures = 2^0=1s).
	impl := r.(*registryImpl)
	impl.mu.Lock()
	impl.byID[u.ID].LastFailureAt = time.Now().Add(-5 * time.Second)
	impl.mu.Unlock()
	if !r.CanAttempt(u.ID) {
		t.Fatalf("after backoff window elapsed, should attempt again")
	}
}

func TestRefreshCard_InvalidCardRejected(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "", URL: ""}}
	r := New(db, f)
	u, _ := r.Add(ctx, AddInput{Name: "u", BaseURL: "http://u"})
	err := r.RefreshCard(ctx, u.ID)
	if err == nil {
		t.Fatalf("expected invalid card error")
	}
}

func TestRefreshCard_FetchFailureClearsStaleCard(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "fake", URL: "http://u"}}
	r := New(db, f)
	u, err := r.Add(ctx, AddInput{Name: "u", BaseURL: "http://u"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, ok := r.Get(u.ID)
	if !ok {
		t.Fatalf("Get after add: not found")
	}
	if got.Card == nil {
		t.Fatalf("expected cached card after successful add refresh")
	}
	if got.Status != store.StatusHealthy {
		t.Fatalf("expected healthy after initial fetch, got %s", got.Status)
	}

	drain(r.Events())
	f.err = errors.New("upstream down")
	if err := r.RefreshCard(ctx, u.ID); err == nil {
		t.Fatalf("expected refresh error")
	}
	got, ok = r.Get(u.ID)
	if !ok {
		t.Fatalf("Get after refresh failure: not found")
	}
	if got.Card != nil {
		t.Fatalf("expected stale card to be cleared after refresh failure")
	}
	if got.Status != store.StatusUnknown {
		t.Fatalf("expected status unknown after stale-card eviction, got %s", got.Status)
	}

	select {
	case ev := <-r.Events():
		if ev.Kind != EventCardChanged {
			t.Fatalf("expected EventCardChanged, got %v", ev.Kind)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("expected card-changed event after stale-card eviction")
	}
}

// drain reads any pending events on ch until it blocks briefly.
func drain(ch <-chan Event) {
	for {
		select {
		case <-ch:
		case <-time.After(10 * time.Millisecond):
			return
		}
	}
}

func TestRefreshCard_UnhealthyBeforeBackoff_StaysUnhealthy(t *testing.T) {
	// A successful card fetch during the backoff window (just after 3
	// failures) must NOT resurrect the upstream — that would flap on
	// upstreams that come and go every few seconds.
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "u", URL: "http://u"}}
	r := New(db, f)
	u, err := r.Add(ctx, AddInput{Name: "u", BaseURL: "http://u"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Trip the breaker: 3 failures → unhealthy with backoff = 1s.
	for i := 0; i < 3; i++ {
		r.RecordFailure(u.ID, errors.New("boom"))
	}
	got, _ := r.Get(u.ID)
	if got.Status != store.StatusUnhealthy {
		t.Fatalf("expected unhealthy after 3 failures, got %s", got.Status)
	}
	// LastFailureAt is "now", so the 1s window has not elapsed.
	if err := r.RefreshCard(ctx, u.ID); err != nil {
		t.Fatalf("RefreshCard: %v", err)
	}
	got, _ = r.Get(u.ID)
	if got.Status != store.StatusUnhealthy {
		t.Fatalf("unhealthy upstream healed too eagerly during backoff: %s", got.Status)
	}
}

func TestRefreshCard_UnhealthyAfterBackoff_HealsAndResetsFailures(t *testing.T) {
	// After the backoff window elapses, a successful card fetch should
	// restore healthy status and clear the failure counter, breaking the
	// composite-card discovery deadlock.
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "u", URL: "http://u"}}
	r := New(db, f)
	u, err := r.Add(ctx, AddInput{Name: "u", BaseURL: "http://u"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	for i := 0; i < 3; i++ {
		r.RecordFailure(u.ID, errors.New("boom"))
	}
	// Rewind LastFailureAt so the 1s backoff window has definitely elapsed.
	impl := r.(*registryImpl)
	impl.mu.Lock()
	impl.byID[u.ID].LastFailureAt = time.Now().Add(-5 * time.Second)
	impl.mu.Unlock()

	if err := r.RefreshCard(ctx, u.ID); err != nil {
		t.Fatalf("RefreshCard: %v", err)
	}
	got, _ := r.Get(u.ID)
	if got.Status != store.StatusHealthy {
		t.Fatalf("expected healthy after post-backoff refresh, got %s", got.Status)
	}
	if got.ConsecutiveFailures != 0 {
		t.Fatalf("expected ConsecutiveFailures reset to 0, got %d", got.ConsecutiveFailures)
	}
}

func TestAdd_ReAddAfterRemove_ReusesDBID(t *testing.T) {
	// Regression: Remove soft-deletes (enabled=false) in DB but removes from
	// memory. A subsequent Add generates a new UUID that wins in-memory but
	// loses to UpsertUpstream's ON CONFLICT(name) in the DB, leaving the DB
	// row with the old id. Dispatch then fails with a FK constraint error
	// because CreateTask references the in-memory id that doesn't exist in
	// the upstreams table.
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "u", URL: "http://u"}}
	r := New(db, f)

	// Step 1: Add an upstream.
	original, err := r.Add(ctx, AddInput{Name: "u", BaseURL: "http://u"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	origID := original.ID

	// Step 2: Remove it (soft-delete in DB, gone from memory).
	if err := r.Remove(ctx, origID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := r.Get(origID); ok {
		t.Fatalf("expected upstream to be gone from memory after Remove")
	}
	// DB row should still exist (disabled).
	row, err := db.GetUpstreamByName(ctx, "u")
	if err != nil {
		t.Fatalf("DB row should still exist after soft-delete: %v", err)
	}
	if row.ID != string(origID) {
		t.Fatalf("DB row id mismatch: got %s, want %s", row.ID, origID)
	}

	// Step 3: Re-add the same name.
	drain(r.Events())
	readded, err := r.Add(ctx, AddInput{Name: "u", BaseURL: "http://u2"})
	if err != nil {
		t.Fatalf("re-Add: %v", err)
	}
	// The in-memory id must match the DB id (the original one).
	if readded.ID != origID {
		t.Fatalf("re-Add id mismatch: in-memory=%s, want=%s (original DB id)", readded.ID, origID)
	}
	// Verify via Get as well.
	got, ok := r.Get(origID)
	if !ok {
		t.Fatalf("Get(origID) should find the re-added upstream")
	}
	if got.BaseURL != "http://u2" {
		t.Fatalf("expected updated base_url, got %s", got.BaseURL)
	}
	// And the DB row should also match.
	row2, err := db.GetUpstreamByName(ctx, "u")
	if err != nil {
		t.Fatalf("DB lookup after re-add: %v", err)
	}
	if row2.ID != string(origID) {
		t.Fatalf("DB id should still be the original: got %s, want %s", row2.ID, origID)
	}
	if row2.BaseURL != "http://u2" {
		t.Fatalf("DB base_url should be updated: got %s", row2.BaseURL)
	}

	// Step 4: Verify CreateTask would work (simulate FK check).
	if err := db.CreateTask(ctx, "test-task-1", "ctx-1", string(origID)); err != nil {
		t.Fatalf("CreateTask with persisted id should succeed: %v", err)
	}
}

func TestRefreshCard_UnknownBecomesHealthy_Preserved(t *testing.T) {
	// Regression guard: the unknown → healthy transition on a successful
	// first fetch must survive the RefreshCard rewrite.
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "u", URL: "http://u"}}
	r := New(db, f)
	u, err := r.Add(ctx, AddInput{Name: "u", BaseURL: "http://u"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Add triggers an initial fetch that flips unknown → healthy.
	got, _ := r.Get(u.ID)
	if got.Status != store.StatusHealthy {
		t.Fatalf("expected healthy after initial fetch, got %s", got.Status)
	}
}
