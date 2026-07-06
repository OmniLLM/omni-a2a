// Package store provides typed CRUD access to the omni-agent-hub hub's SQLite state.
//
// Tables (see schema.sql):
//   - upstreams:   persistent copy of the registry, incl. health & card cache
//   - tasks:       hub-side task rows (one row per hub_task_id)
//   - task_id_map: separates hub-visible IDs from upstream-issued IDs
//   - audit_log:   append-only dispatch events, capped on startup
//
// This package does no logging beyond structured slog when errors occur inside
// helper methods; higher layers own request-context logging.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

//go:embed schema.sql
var schemaFS embed.FS

// currentSchemaVersion is the PRAGMA user_version we bump every time we add a
// migration. Start at v1.
const currentSchemaVersion = 1

// Store is a thin, typed wrapper around a *sql.DB.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path in WAL mode and runs
// any pending migrations up to currentSchemaVersion. The parent directory
// is created if it doesn't exist.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating store dir %s: %w", dir, err)
		}
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite %s: %w", path, err)
	}
	// Single connection prevents "database is locked" surprises in a low-QPS
	// service. Reads/writes are naturally serialized inside SQLite.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying *sql.DB for callers that need to run bespoke queries
// (rare — most access should go through methods on Store).
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("reading user_version: %w", err)
	}
	if version >= currentSchemaVersion {
		return nil
	}
	// v0 → v1: run the embedded schema.sql.
	if version == 0 {
		schema, err := schemaFS.ReadFile("schema.sql")
		if err != nil {
			return fmt.Errorf("loading embedded schema: %w", err)
		}
		if err := runInTx(s.db, func(tx *sql.Tx) error {
			if _, err := tx.Exec(string(schema)); err != nil {
				return err
			}
			if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", currentSchemaVersion)); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return fmt.Errorf("applying v1 schema: %w", err)
		}
	}
	return nil
}

func runInTx(db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// --- Time helpers -----------------------------------------------------------

// nowUTC returns the current time as an ISO-8601 UTC string. Every timestamp
// in the schema is stored as TEXT for portability.
func nowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// nullTimeString converts a *time.Time into a sql.NullString.
func nullTimeString(t *time.Time) sql.NullString {
	if t == nil || t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: t.UTC().Format(time.RFC3339Nano), Valid: true}
}

// parseTime parses an ISO-8601 string into a *time.Time (nil on empty/invalid).
func parseTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		return nil
	}
	return &t
}

// --- Common errors ---------------------------------------------------------

// ErrNotFound is returned by lookup helpers when the target row is absent.
var ErrNotFound = errors.New("store: not found")

// withCtx is a small helper so every method can bail out on a canceled context.
func (s *Store) withCtx(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
