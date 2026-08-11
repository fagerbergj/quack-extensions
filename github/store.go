package github

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	// glebarez/go-sqlite, not modernc.org/sqlite directly: quack itself
	// already imports glebarez/go-sqlite (transitively via glebarez/sqlite's
	// GORM dialector, internal/store.go) to register the "sqlite"
	// database/sql driver - importing modernc.org/sqlite here too would
	// register the SAME driver name a second time and panic at init
	// ("sql: Register called twice for driver sqlite"). glebarez/go-sqlite
	// is itself a modernc.org/sqlite wrapper, so this is the same engine,
	// not a different one.
	_ "github.com/glebarez/go-sqlite"
)

// ghStore is this extension's private persistence for the four
// GitHub-specific tables that used to live in quack's shared Postgres
// store: snapshot, review baseline, CI-fix state, and merge intent
// (design doc Risk 2 - extension-owned storage loses that shared
// connection/transaction surface). SQLite over a mutexed JSON blob: four
// independently-keyed tables with their own read-modify-write patterns
// don't belong serialized behind one file-wide lock.
type ghStore struct {
	db *sql.DB
}

const ghSchema = `
CREATE TABLE IF NOT EXISTS github_snapshot (
	chat_id TEXT PRIMARY KEY,
	json TEXT NOT NULL,
	updated_at TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS github_review_baseline (
	chat_id TEXT PRIMARY KEY,
	patch_ids TEXT NOT NULL,
	updated_at TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS github_fix_state (
	chat_id TEXT PRIMARY KEY,
	last_sha TEXT NOT NULL,
	stopped INTEGER NOT NULL,
	updated_at TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS github_merge_intent (
	chat_id TEXT PRIMARY KEY,
	requested_by TEXT NOT NULL,
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL
);
`

// openStore opens (creating and migrating if needed) the extension's
// SQLite database under dataDir. MaxOpenConns(1) is deliberate: this is a
// low-volume, extension-local store, not quack's shared Postgres pool - one
// connection makes every statement serialize through database/sql's own
// pool, which sidesteps SQLITE_BUSY under concurrent writers entirely
// instead of tuning WAL/busy_timeout (see store_test.go's concurrency test).
func openStore(dataDir string) (*ghStore, error) {
	path := filepath.Join(dataDir, "github.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("github: open store: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(ghSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("github: migrate store: %w", err)
	}
	return &ghStore{db: db}, nil
}

func (s *ghStore) Close() error { return s.db.Close() }

// GetSnapshot returns the stored snapshot JSON, or ("", false, nil) when none exists.
func (s *ghStore) GetSnapshot(ctx context.Context, chatID string) (string, bool, error) {
	var j string
	err := s.db.QueryRowContext(ctx, `SELECT json FROM github_snapshot WHERE chat_id = ?`, chatID).Scan(&j)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return j, true, nil
}

// SetSnapshot upserts the snapshot JSON for the next resume's diff.
func (s *ghStore) SetSnapshot(ctx context.Context, chatID, json string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO github_snapshot (chat_id, json, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET json = excluded.json, updated_at = excluded.updated_at`,
		chatID, json, time.Now().UTC())
	return err
}

// GetReviewBaseline returns the patch-id list quack last delivered a review at.
func (s *ghStore) GetReviewBaseline(ctx context.Context, chatID string) (string, bool, error) {
	var p string
	err := s.db.QueryRowContext(ctx, `SELECT patch_ids FROM github_review_baseline WHERE chat_id = ?`, chatID).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return p, true, nil
}

// SetReviewBaseline upserts the patch-id list (only when a review is delivered).
func (s *ghStore) SetReviewBaseline(ctx context.Context, chatID, patchIDsJSON string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO github_review_baseline (chat_id, patch_ids, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET patch_ids = excluded.patch_ids, updated_at = excluded.updated_at`,
		chatID, patchIDsJSON, time.Now().UTC())
	return err
}

// FixState tracks the CI auto-heal loop bound for one PR chat.
type FixState struct {
	ChatID  string
	LastSHA string
	Stopped bool
}

// GetFixState returns the auto-heal state, or (nil, nil) when none exists.
func (s *ghStore) GetFixState(ctx context.Context, chatID string) (*FixState, error) {
	var fs FixState
	var stopped int
	err := s.db.QueryRowContext(ctx, `SELECT chat_id, last_sha, stopped FROM github_fix_state WHERE chat_id = ?`, chatID).
		Scan(&fs.ChatID, &fs.LastSHA, &stopped)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	fs.Stopped = stopped != 0
	return &fs, nil
}

// SetFixState upserts the auto-heal state (persisted before the fix run so a crash doesn't refund it).
func (s *ghStore) SetFixState(ctx context.Context, fs FixState) error {
	stopped := 0
	if fs.Stopped {
		stopped = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO github_fix_state (chat_id, last_sha, stopped, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET last_sha = excluded.last_sha, stopped = excluded.stopped, updated_at = excluded.updated_at`,
		fs.ChatID, fs.LastSHA, stopped, time.Now().UTC())
	return err
}

// DeleteFixState re-arms auto-heal (human re-applied the fix label).
func (s *ghStore) DeleteFixState(ctx context.Context, chatID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM github_fix_state WHERE chat_id = ?`, chatID)
	return err
}

// MergeIntent records a standing merge authorization for a PR chat, durable
// across restarts so quack:merge applied before a review still works.
type MergeIntent struct {
	ChatID      string
	RequestedBy string
	CreatedAt   time.Time
}

// GetMergeIntent returns the merge authorization, or (nil, nil) when none.
func (s *ghStore) GetMergeIntent(ctx context.Context, chatID string) (*MergeIntent, error) {
	var mi MergeIntent
	err := s.db.QueryRowContext(ctx, `SELECT chat_id, requested_by, created_at FROM github_merge_intent WHERE chat_id = ?`, chatID).
		Scan(&mi.ChatID, &mi.RequestedBy, &mi.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &mi, nil
}

// SetMergeIntent upserts the merge authorization (quack:merge label applied).
func (s *ghStore) SetMergeIntent(ctx context.Context, chatID, requestedBy string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO github_merge_intent (chat_id, requested_by, created_at, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET requested_by = excluded.requested_by, updated_at = excluded.updated_at`,
		chatID, requestedBy, now, now)
	return err
}

// DeleteMergeIntent clears the merge authorization (consumed by merge).
func (s *ghStore) DeleteMergeIntent(ctx context.Context, chatID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM github_merge_intent WHERE chat_id = ?`, chatID)
	return err
}
