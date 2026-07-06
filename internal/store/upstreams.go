package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
)

// UpstreamSource is a small enum for where an upstream row was created.
type UpstreamSource string

const (
	SourceConfig UpstreamSource = "config"
	SourceAdmin  UpstreamSource = "admin"
)

// HealthStatus mirrors the `status` column values.
type HealthStatus string

const (
	StatusUnknown   HealthStatus = "unknown"
	StatusHealthy   HealthStatus = "healthy"
	StatusUnhealthy HealthStatus = "unhealthy"
)

// UpstreamRow is one row of the `upstreams` table, marshaled/unmarshaled by
// this package.
type UpstreamRow struct {
	ID                  string
	Name                string
	BaseURL             string
	AuthScheme          string
	AuthToken           string
	Prefix              string
	Enabled             bool
	Source              UpstreamSource
	Status              HealthStatus
	ConsecutiveFailures int
	LastSuccessAt       sql.NullString
	LastFailureAt       sql.NullString
	CardJSON            sql.NullString
	CardFetchedAt       sql.NullString
	CreatedAt           string
	UpdatedAt           string
}

// Card returns the parsed AgentCard cached on the row, if present.
func (u UpstreamRow) Card() *a2a.AgentCard {
	if !u.CardJSON.Valid || u.CardJSON.String == "" {
		return nil
	}
	var c a2a.AgentCard
	if err := json.Unmarshal([]byte(u.CardJSON.String), &c); err != nil {
		return nil
	}
	return &c
}

// UpsertUpstream inserts a new upstream row or updates an existing one by name.
// It preserves the row's id (so `tasks.upstream_id` foreign keys stay valid
// across re-adds). The caller is responsible for setting id / created_at when
// inserting a new row.
func (s *Store) UpsertUpstream(ctx context.Context, u UpstreamRow) error {
	now := nowUTC()
	if u.UpdatedAt == "" {
		u.UpdatedAt = now
	}
	if u.CreatedAt == "" {
		u.CreatedAt = now
	}
	const q = `
		INSERT INTO upstreams (
			id, name, base_url, auth_scheme, auth_token, prefix, enabled, source,
			status, consecutive_failures, last_success_at, last_failure_at,
			card_json, card_fetched_at, created_at, updated_at
		) VALUES (
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		)
		ON CONFLICT(name) DO UPDATE SET
			base_url = excluded.base_url,
			auth_scheme = excluded.auth_scheme,
			auth_token = excluded.auth_token,
			prefix = excluded.prefix,
			enabled = excluded.enabled,
			source = CASE
				WHEN upstreams.source = 'admin' THEN 'admin'
				ELSE excluded.source
			END,
			updated_at = excluded.updated_at
	`
	_, err := s.db.ExecContext(s.withCtx(ctx), q,
		u.ID, u.Name, u.BaseURL, u.AuthScheme, u.AuthToken, u.Prefix,
		boolToInt(u.Enabled), string(u.Source), string(u.Status),
		u.ConsecutiveFailures, u.LastSuccessAt, u.LastFailureAt,
		u.CardJSON, u.CardFetchedAt, u.CreatedAt, u.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert upstream %q: %w", u.Name, err)
	}
	return nil
}

// GetUpstreamByName returns an upstream row by human name, or ErrNotFound.
func (s *Store) GetUpstreamByName(ctx context.Context, name string) (UpstreamRow, error) {
	const q = `SELECT ` + upstreamColumns + ` FROM upstreams WHERE name = ?`
	row := s.db.QueryRowContext(s.withCtx(ctx), q, name)
	return scanUpstream(row)
}

// GetUpstreamByID returns an upstream row by id, or ErrNotFound.
func (s *Store) GetUpstreamByID(ctx context.Context, id string) (UpstreamRow, error) {
	const q = `SELECT ` + upstreamColumns + ` FROM upstreams WHERE id = ?`
	row := s.db.QueryRowContext(s.withCtx(ctx), q, id)
	return scanUpstream(row)
}

// ListUpstreams returns all upstream rows. If enabledOnly is true, only enabled
// rows are returned.
func (s *Store) ListUpstreams(ctx context.Context, enabledOnly bool) ([]UpstreamRow, error) {
	q := `SELECT ` + upstreamColumns + ` FROM upstreams`
	if enabledOnly {
		q += ` WHERE enabled = 1`
	}
	q += ` ORDER BY name`
	rows, err := s.db.QueryContext(s.withCtx(ctx), q)
	if err != nil {
		return nil, fmt.Errorf("listing upstreams: %w", err)
	}
	defer rows.Close()
	var out []UpstreamRow
	for rows.Next() {
		u, err := scanUpstream(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUpstreamHealth writes back only the health / card cache columns for
// an upstream. It never touches auth or base_url — those change only via
// UpsertUpstream.
func (s *Store) UpdateUpstreamHealth(ctx context.Context, u UpstreamRow) error {
	const q = `
		UPDATE upstreams
		SET status = ?, consecutive_failures = ?, last_success_at = ?,
		    last_failure_at = ?, card_json = ?, card_fetched_at = ?,
		    updated_at = ?
		WHERE id = ?
	`
	_, err := s.db.ExecContext(s.withCtx(ctx), q,
		string(u.Status), u.ConsecutiveFailures, u.LastSuccessAt, u.LastFailureAt,
		u.CardJSON, u.CardFetchedAt, nowUTC(), u.ID,
	)
	if err != nil {
		return fmt.Errorf("update upstream health id=%s: %w", u.ID, err)
	}
	return nil
}

// SetUpstreamEnabled marks the row enabled/disabled without touching health or
// card. Used by the admin DELETE endpoint (soft-delete).
func (s *Store) SetUpstreamEnabled(ctx context.Context, id string, enabled bool) error {
	_, err := s.db.ExecContext(s.withCtx(ctx),
		`UPDATE upstreams SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), nowUTC(), id)
	if err != nil {
		return fmt.Errorf("set enabled id=%s: %w", id, err)
	}
	return nil
}

// --- scan helpers ----------------------------------------------------------

const upstreamColumns = `
	id, name, base_url, auth_scheme, auth_token, prefix, enabled, source,
	status, consecutive_failures, last_success_at, last_failure_at,
	card_json, card_fetched_at, created_at, updated_at
`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUpstream(r rowScanner) (UpstreamRow, error) {
	var u UpstreamRow
	var enabled int
	var authTokenNS, prefixNS sql.NullString
	err := r.Scan(
		&u.ID, &u.Name, &u.BaseURL, &u.AuthScheme, &authTokenNS, &prefixNS,
		&enabled, &u.Source, &u.Status, &u.ConsecutiveFailures,
		&u.LastSuccessAt, &u.LastFailureAt,
		&u.CardJSON, &u.CardFetchedAt, &u.CreatedAt, &u.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return UpstreamRow{}, ErrNotFound
	}
	if err != nil {
		return UpstreamRow{}, fmt.Errorf("scan upstream: %w", err)
	}
	u.Enabled = enabled != 0
	if authTokenNS.Valid {
		u.AuthToken = authTokenNS.String
	}
	if prefixNS.Valid {
		u.Prefix = prefixNS.String
	}
	return u, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
