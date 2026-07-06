# A2A Hub / Aggregator — Design

**Date:** 2026-07-05
**Status:** Approved (owner: James Zhu)
**Repo:** `github.com/OmniLLM/omni-agent-hub`
**Delta from today:** in-place redesign of the existing `omni-agent-hub` binary from a "gateway + built-in Hermes executor" into a **pure A2A hub** — a daemon that aggregates any number of upstream A2A agents, exposes a single composite A2A endpoint to clients, and does so with per-agent health, task-ID translation, SSE relay, and admin-time reconfiguration.

## 1. Goals

- **One endpoint per client.** A client that speaks A2A points at the hub's URL and never has to know that ten upstream agents live behind it.
- **Composite Agent Card.** `GET /.well-known/agent-card.json` returns a card whose `skills[]` is the namespaced union of every healthy upstream's skills.
- **Deterministic routing.** Given a `skillId` (or an `@name` mention, or a text prefix), routing to the correct upstream is a pure function.
- **Correctness across multi-turn tasks.** A client that starts a task and follows up with `input-required` state must land on the *same* upstream, using the *same* upstream task ID — but the hub never leaks upstream IDs to the client.
- **Streaming works.** `tasks/sendSubscribe` must be a transparent SSE relay — events reach the client with minimal added latency; upstream disconnects surface as clean terminal events, not hung sockets.
- **Restart-safe daemon.** Upstream registrations, health snapshots, and in-flight task-ID mappings survive a hub restart.
- **CGO-free build.** Static single-binary distribution (`go build ./cmd/omni-agent-hub`), no C toolchain required.

## 2. Non-goals

- **No local execution.** The current in-process Hermes executor is removed. If you want a Hermes agent behind the hub, you run it as its own A2A server (separate binary — a follow-up sub-project) and register it via config or admin API. The hub is a pure aggregator.
- **No routing intelligence.** No load-balancing across two agents that offer the same skill; no LLM-in-the-loop routing. Skills are 1:1 to upstreams. If two upstreams register the same skill ID, later registration loses (and both are logged).
- **No push notifications.** The `pushNotifications` capability is advertised only if an upstream supports it *and* the client is talking about a task owned by that upstream. The hub itself does not originate pushes.
- **No client-facing multi-tenancy.** Single client API key today; per-agent isolation between multiple clients is out of scope.
- **No active health pinger.** Health status is derived from real request outcomes only.

## 3. Architecture Overview

Six internal packages plus `cmd/omni-agent-hub`, with a strict top-down dependency order (no cycles):

```
cmd/omni-agent-hub/                     entry point, cobra CLI, wires packages together
internal/
  a2a/         protocol types    JSON-RPC envelopes + AgentCard + Task types
  config/      YAML loader        server / logging / upstream sections
  store/       SQLite persistence upstreams, tasks, task_id_map, audit_log
  registry/    upstream lifecycle add/remove/list/refresh + health state + change events
  card/        composite card     subscribes to registry, caches AgentCard, O(1) reads
  router/      pure lookup        (skillID | text) -> upstreamID; no I/O, easy to test
  dispatch/    request proxying   Unary (message/send + tasks/*) and Stream (SSE relay)
  transport/   HTTP surface       JSON-RPC handler, admin API, /.well-known/*, /health
  logging/     structured logs    slog + file + tail (existing code, upgraded)
  cli/         cobra commands     serve / start / stop / status / logs / upstream / admin
```

**Dependency direction (arrows are "imports"):**

```
transport ──┬──> dispatch ──> store
            ├──> card ──> registry ──> store
            └──> router (pure — depends only on a2a types)

registry emits change events on a chan; card subscribes.
dispatch emits health updates on a chan; registry subscribes.
```

**One-line contract per package:**

- `a2a` — protocol types and JSON-RPC helpers; no logic.
- `config` — read/write `~/.config/omni-agent-hub/config.yaml`.
- `store` — thin, typed CRUD over SQLite: upstreams, tasks, task_id_map, audit_log. Also exposes `LookupContext(contextID) (upstreamID, bool)` for router stickiness — returns the upstream of the most recent non-terminal task with that contextId.
- `registry` — the authoritative in-memory upstream list + card cache + health state; notifies subscribers on any change.
- `card` — the current composite `AgentCard`; regenerated on registry events.
- `router` — pure resolver: `Resolve(req) -> (upstreamID, remainingText)`.
- `dispatch` — proxy engine; hides task-ID translation, retries, SSE relay from callers.
- `transport` — HTTP handlers only; no business logic; every handler is <30 lines.

**Concurrency model.**
- `registry` protects its map with a `sync.RWMutex`. All mutations go through methods that emit an event on a buffered `chan struct{}` (capacity 1, non-blocking drop). `card` consumes those events in a single goroutine and rebuilds; readers never block.
- `dispatch` uses `context.Context` end-to-end. Timeouts: 30 s connect, 300 s response for unary; no wall-clock cap on streams (only client-context cancellation).
- No goroutine leaks: every stream registers a `defer close(ch)` and honors both client and upstream disconnect.

## 4. Data Model (SQLite)

Pure-Go driver: `modernc.org/sqlite`. Database file: `~/.omni-agent-hub/state.db` (configurable). WAL mode. All timestamps stored as ISO-8601 text UTC.

```sql
-- Upstreams: persistent copy of the registry.
-- Source-of-truth ordering: on startup, config.yaml entries overlay the DB
-- (config wins on conflict for `url`/`token`, DB retains health state).
CREATE TABLE upstreams (
    id              TEXT PRIMARY KEY,            -- stable UUID, generated on first insert
    name            TEXT NOT NULL UNIQUE,        -- human name, used in skill namespacing
    base_url        TEXT NOT NULL,
    auth_scheme     TEXT NOT NULL DEFAULT 'bearer',  -- 'bearer' | 'none'
    auth_token      TEXT,                        -- opaque; secret-encoded in future
    prefix          TEXT,                        -- optional @-mention override
    enabled         INTEGER NOT NULL DEFAULT 1,
    source          TEXT NOT NULL,               -- 'config' | 'admin'
    -- health & card cache
    status          TEXT NOT NULL DEFAULT 'unknown',  -- 'healthy' | 'unhealthy' | 'unknown'
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    last_success_at TEXT,
    last_failure_at TEXT,
    card_json       TEXT,                        -- last-known good AgentCard as JSON
    card_fetched_at TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);
CREATE INDEX idx_upstreams_enabled ON upstreams(enabled);

-- Tasks: hub-side task rows. One row per hub_task_id.
-- Terminal states are kept for `tasks/get` history (TTL cleanup on startup: 7d default).
CREATE TABLE tasks (
    hub_task_id     TEXT PRIMARY KEY,            -- UUID we mint and return to the client
    context_id      TEXT NOT NULL,               -- A2A contextId (may span multiple tasks)
    upstream_id     TEXT NOT NULL REFERENCES upstreams(id),
    state           TEXT NOT NULL,               -- submitted | working | input-required | completed | canceled | failed
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    -- last known snapshot of the Task object as returned to the client, for tasks/get replay
    last_task_json  TEXT
);
CREATE INDEX idx_tasks_context ON tasks(context_id);
CREATE INDEX idx_tasks_upstream ON tasks(upstream_id);
CREATE INDEX idx_tasks_state ON tasks(state);

-- Task-ID map: separates hub-visible IDs from upstream-issued IDs.
-- Never expose upstream_task_id to the client.
CREATE TABLE task_id_map (
    hub_task_id       TEXT PRIMARY KEY REFERENCES tasks(hub_task_id) ON DELETE CASCADE,
    upstream_id       TEXT NOT NULL REFERENCES upstreams(id),
    upstream_task_id  TEXT NOT NULL,
    UNIQUE(upstream_id, upstream_task_id)
);

-- Audit log: dispatch events (send, forward, resp, error, cancel, breaker-open, breaker-close).
-- Rolls forward, capped at N rows via a startup vacuum (default 10k).
CREATE TABLE audit_log (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    ts           TEXT NOT NULL,
    trace_id     TEXT,
    hub_task_id  TEXT,
    upstream_id  TEXT,
    event        TEXT NOT NULL,
    detail_json  TEXT
);
CREATE INDEX idx_audit_ts ON audit_log(ts);
CREATE INDEX idx_audit_task ON audit_log(hub_task_id);
```

**Migrations.** Managed inline in `store.Open()` via `PRAGMA user_version` — start at v1 (the schema above). Every future schema change increments `user_version` and runs its migration inside a single transaction.

## 5. Registry: Upstream Lifecycle

**Types.**

```go
package registry

type UpstreamID string   // the stable id from `upstreams.id`

type Upstream struct {
    ID       UpstreamID
    Name     string
    BaseURL  string
    Auth     AuthConfig
    Prefix   string
    Enabled  bool
    Source   Source            // ConfigSource | AdminSource
    Status   HealthStatus      // Healthy | Unhealthy | Unknown
    Card     *a2a.AgentCard    // last-known good; nil if never fetched
    // health counters (in-memory, mirrored to store)
    ConsecutiveFailures int
    LastSuccessAt, LastFailureAt time.Time
    CardFetchedAt time.Time
}

type Registry interface {
    // Query
    List() []Upstream
    Get(id UpstreamID) (Upstream, bool)
    GetByName(name string) (Upstream, bool)

    // Mutation
    Add(ctx context.Context, in AddInput) (Upstream, error)      // admin path
    Remove(ctx context.Context, id UpstreamID) error             // admin path
    RefreshCard(ctx context.Context, id UpstreamID) error        // admin path or on-demand
    RefreshAll(ctx context.Context) error                        // startup + admin refresh

    // Health signals (called by dispatch)
    RecordSuccess(id UpstreamID)
    RecordFailure(id UpstreamID, err error)

    // Change notification
    Events() <-chan Event                                        // buffered=1, non-blocking send
}

type Event struct {
    Kind EventKind   // Added | Removed | Updated | HealthChanged | CardChanged
    ID   UpstreamID
}
```

**Registration flow (both static and admin).**

1. Insert/update the `upstreams` row (status=`unknown`).
2. Fetch `GET {base_url}/.well-known/agent-card.json` (fall back to `/.well-known/agent.json`). 5 s timeout.
3. Validate: card MUST have `name`, `url`, and a `skills` array (may be empty). Malformed → reject with a specific error; row stays with `status=unhealthy` and `card_json=NULL`. Never quarantines the whole startup.
4. On success: cache the card JSON, set `card_fetched_at`, status → `healthy`, emit `CardChanged`.

**Health / circuit breaker.**

- A "failure" for breaker purposes is: network error, timeout, HTTP 5xx, HTTP 502/503/504, or a malformed (non-JSON-RPC) 200 response. HTTP 4xx (except 502/503/504) is *not* a failure — it's a client-caused error and does not count toward breaker state.
- **3 consecutive failures** → `status=unhealthy`, exclude the upstream's skills from the composite card immediately (emit `HealthChanged`).
- **On next request**, retry with an exponential backoff gate: retry allowed at `2^min(failures-3, 6) * 1s` intervals (1s → 64s ceiling). Requests within that window fail-fast with a hub-generated `unavailable` JSON-RPC error.
- **1 success** → reset counter, status → `healthy`, emit `HealthChanged`.
- No background pinger. Health is a pure side-effect of real traffic.
- Sticky (context-routed) requests bypass the "exclude from composite card" but still respect the breaker window — so a client trying to continue a task against a dead upstream gets a clean `Upstream unavailable` error instead of routing to a *different* upstream.

**Card re-fetch.** No periodic ticker. On any admin `RefreshCard`, or if a live request response includes a header the upstream sets (`A2A-Card-Version` — best-effort), enqueue a card refetch in a single background worker goroutine. Idempotent.

**Config vs admin precedence.** On startup, config.yaml entries are upserted with `source='config'`. Admin-added entries (`source='admin'`) are only removed by admin request. Config entries that disappear from `config.yaml` are marked `enabled=0` on the next startup but their history is retained.

## 6. Composite Agent Card

**Package `card`** owns a single value: the current composite `AgentCard`. It exposes:

```go
type Builder interface {
    Current() a2a.AgentCard        // O(1), lock-free read of an atomic.Pointer
    Rebuild()                      // called by an internal goroutine on registry events
}
```

Internally: an `atomic.Pointer[a2a.AgentCard]` swapped on rebuild. Handlers do `builder.Current()` and never block.

**Rebuild trigger.** A single goroutine `select`s on `registry.Events()`; on any event it debounces (100 ms coalescing timer) and calls the rebuild routine. Coalescing prevents thrash if config.yaml declares 20 upstreams and they all card-refresh during startup.

**Rebuild logic.**

```
skills := []AgentSkill{}
for each registry upstream u where u.Status == Healthy and u.Card != nil:
    for each s in u.Card.Skills:
        namespacedID := fmt.Sprintf("%s.%s", u.Name, s.ID)  // dot separator
        skills = append(skills, AgentSkill{
            ID:          namespacedID,
            Name:        s.Name,                             // human name unchanged
            Description: s.Description,
            // per-skill capabilities: mirror the OWNING upstream's, never inflated
            Capabilities: u.Card.Capabilities,
        })

// Hub's own top-level capabilities are the UNION of all healthy upstreams'.
caps := AgentCapabilities{
    Streaming:         any(u.Card.Capabilities.Streaming)
    PushNotifications: any(u.Card.Capabilities.PushNotifications)
}

card := AgentCard{
    Name:               cfg.Hub.Name,               // from `hub.name`
    Description:        cfg.Hub.Description,        // from `hub.description`
    URL:                cfg.Server.PublicURL,       // from `server.public_url`
    Version:            build.Version,
    Capabilities:       caps,
    Authentication:     hubAuthScheme(cfg),         // "bearer" if api_key set
    DefaultInputModes:  []string{"text/plain"},
    DefaultOutputModes: []string{"text/plain"},
    Skills:             skills,
}
```

**Collision policy.** If two upstreams `a` and `b` both register a skill with the same *original* ID `x`, namespacing produces `a.x` and `b.x` — no collision. If two upstreams share the same `Name`, `registry.Add` rejects the second registration; there is no silent overwrite.

## 7. Router

Pure, single-file package. Zero I/O. Fully unit-testable without a mock upstream.

```go
package router

type Request struct {
    SkillID   string    // optional; wins if set and known
    Text      string    // first user-message text; used for @mention and prefix fallbacks
    ContextID string    // A2A contextId; if present and mapped to an existing task, sticks routing
}

type Resolution struct {
    UpstreamID   registry.UpstreamID
    // If routing rewrote the message (e.g. stripped an @mention), the caller
    // uses RewrittenText instead of the original.
    RewrittenText string
    // If SkillID was namespaced (e.g. "hermes.coding"), the upstream-scoped
    // skill ID to forward (e.g. "coding") is returned here. Empty for
    // mention/prefix/context routes — no rewrite needed.
    UpstreamSkillID string
    // Explanation for logging/telemetry.
    Reason string     // "context" | "skill" | "mention" | "prefix"
}

// Snapshot is an immutable view assembled by the caller from registry.List().
// StickyUpstream returns the upstream that owns a prior contextId, if any.
type Snapshot interface {
    Healthy() []registry.Upstream
    ByName(string) (registry.Upstream, bool)
    // StickyUpstream is populated by the caller (from `store.LookupContext`)
    // and read here — router itself does no I/O.
    StickyUpstream(contextID string) (registry.UpstreamID, bool)
}

func Resolve(req Request, snap Snapshot) (Resolution, bool)
```

Resolution order:

1. **Context stickiness.** If `ContextID` is present and `snap.StickyUpstream` returns a known upstream, resolve to it. Reason `"context"`. This guarantees multi-turn tasks (`input-required` → follow-up) always land on the same upstream, regardless of what the client sends in `SkillID` or `Text`. If the sticky upstream is unhealthy, still route to it (breaker check happens in dispatch) — do NOT silently switch upstreams mid-conversation.
2. **Skill match.** If `SkillID` is present and matches `^([^.]+)\.(.+)$`, look up the prefix as an upstream name. If found and healthy, resolve — set `UpstreamSkillID` to the suffix. Un-namespaced skill IDs are legal only if they exist unambiguously in the snapshot's skill index; otherwise return `false`. Reason `"skill"`.
3. **@mention.** If `Text` starts with `@name ` (or `@name` alone), strip and set `RewrittenText`. If the name is a known healthy upstream, resolve. Reason `"mention"`.
4. **Prefix.** For each upstream with a non-empty `Prefix`, `strings.HasPrefix(Text, prefix)` → resolve (do not rewrite). Reason `"prefix"`.
5. Nothing matched → `(_, false)`.

**Why this shape.** `Resolve` returns without ever touching the registry directly — the caller passes in a snapshot. That makes it a deterministic function of its inputs, so unit tests are trivial and the router cannot cause a lock in registry.

## 8. Dispatch

Two entry points, one per interaction pattern.

```go
package dispatch

type Unary interface {
    SendMessage(ctx context.Context, req UnaryRequest) (UnaryResponse, error)
    GetTask(ctx context.Context, hubTaskID string) (*a2a.Task, error)
    CancelTask(ctx context.Context, hubTaskID string) error
}

type Stream interface {
    SendMessageSubscribe(ctx context.Context, req UnaryRequest) (<-chan Event, error)
}

type UnaryRequest struct {
    Res     router.Resolution
    Message a2a.Message
    ContextID string
}
```

**Unary flow — `SendMessage`.**

1. Look up the upstream via `registry.Get(res.UpstreamID)`. If unhealthy and inside the breaker window, return an `Unavailable` error immediately without contacting upstream. Log a `breaker-blocked` audit event.
2. Mint a `hub_task_id` (UUID). Write a placeholder `tasks` row (`state='submitted'`, no upstream_task_id yet).
3. Build the upstream JSON-RPC request, replacing `skillId` with `res.UpstreamSkillID` if router rewrote it, and `message.parts[0].text` with `res.RewrittenText` if router rewrote that.
4. `POST {upstream.base_url}/` with `Authorization: Bearer {upstream.auth_token}`. 300 s timeout.
5. On response: parse the upstream `Task`, extract `id` as `upstream_task_id`, insert into `task_id_map`, update `tasks` with the state and last_task_json. **Rewrite** the returned Task's `id` field to `hub_task_id` before returning to the caller.
6. On failure: `registry.RecordFailure(id, err)`, write an audit_log entry, return `Upstream error` JSON-RPC code.
7. On success: `registry.RecordSuccess(id)`.

**Unary flow — `GetTask`.**

1. Look up `hub_task_id` in `tasks`. Missing → `-32001 Task not found`.
2. If state is terminal (`completed`/`failed`/`canceled`): return the cached `last_task_json` (no upstream call).
3. Else: look up `upstream_task_id` via `task_id_map`, forward `tasks/get` to that upstream with the mapped ID, rewrite response ID to hub_task_id, persist updated `last_task_json`.

**Unary flow — `CancelTask`.** Same as GetTask but forwards `tasks/cancel`. If the upstream returns "cancel not supported," pass through the error as-is (the hub does not fabricate cancels).

**Stream flow — `SendMessageSubscribe`.**

1. Resolve, breaker-check, mint hub_task_id exactly like unary.
2. `POST {upstream.base_url}/` with the JSON-RPC body and `Accept: text/event-stream`. Read the response as SSE.
3. Spawn a single goroutine that reads events from the upstream's response body and pushes them onto the returned channel. Every event's payload has its `id` field rewritten from `upstream_task_id` to `hub_task_id`.
4. **Terminate correctly:**
   - Client context cancelled → close upstream body, close channel, exit goroutine.
   - Upstream disconnects mid-stream → emit a synthesized `TaskStatusUpdateEvent{state:"failed", message:"upstream disconnected"}` on the channel, close channel, exit.
   - Terminal event received (state completed/failed/canceled) → pass through, close channel.
5. Every event triggers a `tasks.state` update in SQLite (best-effort, non-blocking; failures logged but not returned to the client).
6. No buffering: events are forwarded as soon as they arrive. The channel is unbuffered — writes block until the transport writes to the client.

**Task-ID translation is centralized here.** Nowhere else in the codebase does upstream_task_id appear. Handler code, router code, and card code have no way to reach it.

## 9. Transport (HTTP)

Handlers are thin. Each handler:
1. Parses the request into a strong type.
2. Calls one method on `dispatch`, `registry`, or `card.Current()`.
3. Serializes and writes.

**Public endpoints (no auth):**

- `GET /health` — `{"status":"ok","upstreams":{"total":N,"healthy":M}}`
- `GET /.well-known/agent-card.json` — returns `card.Current()`
- `GET /.well-known/agent.json` — same handler (compat alias)
- `GET /metrics` — Prometheus text format (see §10)

**Client endpoints (client API-key auth):**

- `POST /` — A2A JSON-RPC. Dispatched by method:
  - `message/send` → `dispatch.SendMessage`
  - `message/sendSubscribe` → `dispatch.SendMessageSubscribe` (upgrades to SSE)
  - `tasks/get` → `dispatch.GetTask`
  - `tasks/cancel` → `dispatch.CancelTask`
  - `agent/getAuthenticatedExtendedCard` → returns `card.Current()`
  - unknown → `-32601 Method not found`

**Admin endpoints (admin API-key auth — distinct token):**

- `GET /admin/upstreams` — list all with health + card presence
- `POST /admin/upstreams` — body: `{name, base_url, auth: {scheme, token}, prefix}` → 201 with the created record. Persisted with `source='admin'`.
- `DELETE /admin/upstreams/{id}` — 204. Marks the upstream `enabled=0` and removes it from the in-memory registry (so no new routing decisions can pick it). The `upstreams` row is retained (soft-delete) so `tasks.upstream_id` foreign keys stay valid and `tasks/get` for historical tasks still works. A subsequent re-add with the same `name` reuses the same `id`. In-flight requests against the upstream at the time of deletion are allowed to complete (up to their timeout).
- `POST /admin/upstreams/{id}/refresh` — re-fetch the card. 200 with the new card.
- `POST /admin/refresh` — re-fetch all cards.
- `GET /admin/skills` — flat skill index for debugging.

**Auth middleware.** Two-tier: `apiKeyAuth(cfg.Server.APIKey)` guards `/`, `adminAuth(cfg.Server.AdminKey)` guards `/admin/*`. Both accept `Authorization: Bearer …` and `X-API-Key: …`. If the corresponding key is empty, the middleware **denies all requests** (fail-safe; never open a rewrite endpoint by accident).

**CORS.** Kept as today: `Access-Control-Allow-Origin: *` on all responses, preflight `OPTIONS` handled globally. Admin endpoints also carry CORS headers (assumption: admin UI runs in a local browser).

**Request logging.** Structured `slog` line per request with `trace_id`, method, path, upstream (if resolved), duration, status, bytes in/out. Auth tokens are masked (`first-8-chars****`).

## 10. Observability

- **Structured logs.** All packages log via a shared `*slog.Logger` (JSON handler, sink = stdout + rotating file). Every log entry inside a request carries `trace_id` (UUID minted at transport entry) and, once resolved, `hub_task_id` and `upstream_id`.
- **Metrics** (Prometheus text at `/metrics`):
  - `omni_a2a_requests_total{method,upstream,status}` — counter
  - `omni_a2a_request_duration_seconds{method,upstream}` — histogram
  - `omni_a2a_upstream_healthy{upstream}` — gauge (0/1)
  - `omni_a2a_upstream_consecutive_failures{upstream}` — gauge
  - `omni_a2a_tasks_active` — gauge (count of non-terminal tasks in `tasks` table)
  - `omni_a2a_stream_events_relayed_total{upstream}` — counter
- **Audit log** in SQLite (`audit_log` table) captures every dispatch decision — useful for debugging routing after the fact without needing to grep logs.

## 11. Configuration

`~/.config/omni-agent-hub/config.yaml`:

```yaml
server:
  host: "0.0.0.0"
  port: 8222
  public_url: "http://localhost:8222"   # advertised in composite AgentCard
  api_key: "..."                        # client bearer, required
  admin_key: "..."                      # admin bearer, required for admin API

hub:
  name: "Omni A2A Hub"
  description: "Aggregator for local and remote A2A agents."

storage:
  path: "~/.omni-agent-hub/state.db"          # SQLite file
  audit_retention: 10000                # rows

logging:
  file: "~/.omni-agent-hub/logs/server.log"
  level: "info"                         # debug | info | warn | error
  format: "json"                        # json | text

upstream:
  - name: "hermes"
    base_url: "http://localhost:1424"
    prefix: "@hermes"                   # optional
    auth:
      scheme: "bearer"
      token: "..."
    enabled: true

  - name: "research"
    base_url: "http://localhost:8003"
    auth: { scheme: "none" }
    enabled: true
```

**Breaking changes from today:**
- `agent:` block removed (local Hermes executor extracted).
- New required `server.admin_key`.
- New required `server.public_url`.
- `upstream[].token` moved into nested `upstream[].auth.token`.
- New `storage:` block.

**Migration.** On startup, if a legacy `agent:` or top-level `upstream[].token` is present, log a `WARN` and translate on the fly. `omni-agent-hub config migrate` writes the new shape back to disk.

## 12. CLI (cobra) — no functional change from today's surface

- `omni-agent-hub serve` — foreground
- `omni-agent-hub start / stop / restart / status` — PID-file daemon (unchanged)
- `omni-agent-hub logs [-f] [-n N]` — tail
- `omni-agent-hub upstream list / refresh / add / remove` — talks to the admin API using a local admin token from config
- `omni-agent-hub config migrate` — one-shot migration to the new YAML shape

## 13. Testing Strategy

- **Unit** (fast, no network):
  - `router` — table-driven; every branch of `Resolve` covered.
  - `card` — inputs are registry snapshots, outputs are composite cards; assert namespacing, capability union, exclusion of unhealthy.
  - `store` — golden schema, migration idempotency, task_id_map uniqueness invariant.
  - `dispatch` unit-level — mock upstream via `httptest.Server`; verify task-ID rewrite in both directions, breaker behavior, timeout handling, SSE event rewrite.
- **Integration** (`internal/integration_test`):
  - Boot a real hub against two `httptest.Server` fake upstreams; drive a multi-turn task through `input-required`; assert same upstream, correct rewrite.
  - SSE test: fake upstream that emits three events then closes; assert all three reach the client with hub IDs, terminal `failed` event synthesized on abnormal close.
  - Breaker test: fake upstream returning 500; assert 4th request is fail-fast without a network call.
- **CI** — `go test ./...` gate. `go vet` and `staticcheck` blocking. No `golangci-lint` unless the repo already has it configured.

## 14. Error Handling & Standard Responses

Every JSON-RPC error uses codes:

| Code | Meaning | When |
|---|---|---|
| -32700 | Parse error | Bad JSON on POST / |
| -32600 | Invalid Request | Not JSON-RPC 2.0 |
| -32601 | Method not found | Unknown method |
| -32602 | Invalid params | Bad params for a known method |
| -32001 | Task not found | Unknown hub_task_id |
| -32002 | Upstream HTTP error | Upstream returned non-200 non-JSON |
| -32003 | Invalid upstream response | Upstream returned non-JSON-RPC JSON |
| -32010 | Upstream unavailable | Circuit breaker open |
| -32011 | No route | Router returned no match and no default is configured |
| -32000 | Generic execution error | catch-all with `data:` populated |

For SSE, errors during the stream become synthetic `TaskStatusUpdateEvent` messages so clients don't get bare disconnects.

## 15. Implementation Order (informative — the plan will decompose further)

1. **Skeleton** — new packages, wire `cmd/omni-agent-hub` to call empty implementations; existing endpoints still work by delegating to the old code path.
2. **`store`** — schema + CRUD + migrations. Round-trip tests.
3. **`registry`** — refactor of today's `proxy.UpstreamRegistry` into the new interface; add health counters, breaker, event channel. Persistence via `store`.
4. **`card`** — extract composite-card building out of `server.go`. Add atomic pointer + rebuild goroutine. Existing endpoint delegates to `card.Current()`.
5. **`router`** — pure package, unit tests.
6. **`dispatch.Unary`** — extract forwarding logic; introduce hub_task_id minting and task_id_map. Existing `POST /` migrates to this path. Old code deleted.
7. **`dispatch.Stream`** — SSE relay + `message/sendSubscribe` endpoint (new).
8. **`transport`** — thin the handlers; move all remaining logic out of `server.go`; split `admin/*` under a distinct auth key.
9. **Metrics + audit_log.**
10. **CLI polish** — `omni-agent-hub config migrate`, admin token propagation to `upstream add/remove`.
11. **Delete legacy executor package + `agent:` config.**

## 16. Follow-up sub-projects (not in this spec)

- **Hermes as a standalone A2A server.** Extract `internal/executor/hermes.go` into its own repo/binary that serves the A2A protocol. Registered with the hub via config or admin API. Own design doc.
- **Multi-tenant client auth.** Per-client scoped API keys with per-key routing rules.
- **Secret storage.** Move `auth_token` in SQLite to a real vault (OS keyring or file-based encrypted store).
- **Card-diff optimization.** Only rebuild composite card when the *skill set* changes, not on every health flip.
