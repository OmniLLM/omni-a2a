CREATE TABLE upstreams (
    id                   TEXT PRIMARY KEY,
    name                 TEXT NOT NULL UNIQUE,
    base_url             TEXT NOT NULL,
    auth_scheme          TEXT NOT NULL DEFAULT 'bearer',
    auth_token           TEXT,
    prefix               TEXT,
    enabled              INTEGER NOT NULL DEFAULT 1,
    source               TEXT NOT NULL,
    status               TEXT NOT NULL DEFAULT 'unknown',
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    last_success_at      TEXT,
    last_failure_at      TEXT,
    card_json            TEXT,
    card_fetched_at      TEXT,
    created_at           TEXT NOT NULL,
    updated_at           TEXT NOT NULL
);
CREATE INDEX idx_upstreams_enabled ON upstreams(enabled);

CREATE TABLE tasks (
    hub_task_id    TEXT PRIMARY KEY,
    context_id     TEXT NOT NULL,
    upstream_id    TEXT NOT NULL REFERENCES upstreams(id),
    state          TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    last_task_json TEXT
);
CREATE INDEX idx_tasks_context ON tasks(context_id);
CREATE INDEX idx_tasks_upstream ON tasks(upstream_id);
CREATE INDEX idx_tasks_state ON tasks(state);

CREATE TABLE task_id_map (
    hub_task_id      TEXT PRIMARY KEY REFERENCES tasks(hub_task_id) ON DELETE CASCADE,
    upstream_id      TEXT NOT NULL REFERENCES upstreams(id),
    upstream_task_id TEXT NOT NULL,
    UNIQUE(upstream_id, upstream_task_id)
);

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
