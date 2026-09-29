package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

// ErrInvalidTransition means a branch cannot perform the requested operation.
var ErrInvalidTransition = errors.New("invalid branch transition")

type Event struct {
	ID        int64          `json:"id"`
	TreeID    string         `json:"tree_id,omitempty"`
	BranchID  string         `json:"branch_id,omitempty"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload"`
	CreatedAt time.Time      `json:"created_at"`
}

type SecretRecord struct {
	Name      string
	ValueBlob []byte
	AllowJSON string
	Action    string
	CreatedAt time.Time
}

type TreeRecord struct {
	ID         string
	RootBranch string
	CreatedAt  time.Time
}

type BranchRecord struct {
	ID             string
	TreeID         string
	ParentID       string
	Mode           string
	Status         string
	TokenHash      string
	DivergentCount int64
	CreatedAt      time.Time
	SealedAt       *time.Time
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create store directory: %w", err)
	}
	// Pass the path as-is. SQLite paths may legally contain '?' and '#';
	// appending URI-style pragmas would reinterpret those characters.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.configure(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure sqlite: %w", err)
	}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) configure() error {
	ctx := context.Background()
	for _, pragma := range []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = NORMAL`,
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA foreign_keys = ON`,
	} {
		if _, err := s.db.ExecContext(ctx, pragma); err != nil {
			return err
		}
	}
	return s.db.PingContext(ctx)
}

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS events (
  event_id   INTEGER PRIMARY KEY AUTOINCREMENT,
  tree_id    TEXT,
  branch_id  TEXT,
  type       TEXT NOT NULL,
  payload    TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_created_at ON events(created_at);
CREATE TABLE IF NOT EXISTS secrets (
  name       TEXT PRIMARY KEY,
  value_blob BLOB NOT NULL,
  allow_json TEXT NOT NULL,
  action     TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS trees (
  tree_id     TEXT PRIMARY KEY,
  root_branch TEXT NOT NULL,
  created_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS branches (
  branch_id       TEXT PRIMARY KEY,
  tree_id         TEXT NOT NULL REFERENCES trees(tree_id),
  parent_id       TEXT REFERENCES branches(branch_id),
  mode            TEXT NOT NULL,
  status          TEXT NOT NULL,
  token_hash      TEXT NOT NULL,
  divergent_count INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL,
  sealed_at       INTEGER
);
CREATE INDEX IF NOT EXISTS idx_branches_tree ON branches(tree_id);
CREATE INDEX IF NOT EXISTS idx_branches_token ON branches(token_hash);
CREATE UNIQUE INDEX IF NOT EXISTS idx_branches_token_unique ON branches(token_hash);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	return s.migrateStaging()
}

func (s *Store) RecordEvent(ctx context.Context, treeID, branchID, eventType string, payload map[string]any) error {
	return recordEvent(ctx, s.db, treeID, branchID, eventType, payload)
}

type eventWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func recordEvent(ctx context.Context, writer eventWriter, treeID, branchID, eventType string, payload map[string]any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	_, err = writer.ExecContext(ctx,
		`INSERT INTO events(tree_id, branch_id, type, payload, created_at) VALUES (?, ?, ?, ?, ?)`,
		nullable(treeID), nullable(branchID), eventType, string(encoded), time.Now().UnixNano())
	if err != nil {
		return fmt.Errorf("record event: %w", err)
	}
	return nil
}

func (s *Store) EventCount(ctx context.Context) (int64, error) {
	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count events: %w", err)
	}
	return count, nil
}

// Events returns events with an event_id greater than since, in cursor order.
// The limit is expected to be validated by the API layer.
func (s *Store) Events(ctx context.Context, since, limit int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT event_id, COALESCE(tree_id, ''), COALESCE(branch_id, ''), type, payload, created_at
		FROM events
		WHERE event_id > ?
		ORDER BY event_id ASC
		LIMIT ?`, since, limit)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	events := make([]Event, 0)
	for rows.Next() {
		var event Event
		var payload string
		var createdAt int64
		if err := rows.Scan(&event.ID, &event.TreeID, &event.BranchID, &event.Type, &payload, &createdAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		if err := json.Unmarshal([]byte(payload), &event.Payload); err != nil {
			return nil, fmt.Errorf("decode event payload: %w", err)
		}
		event.CreatedAt = time.Unix(0, createdAt).UTC()
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return events, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) SaveSecret(ctx context.Context, name string, valueBlob []byte, allowJSON, action string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO secrets(name, value_blob, allow_json, action, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET value_blob=excluded.value_blob,
		  allow_json=excluded.allow_json, action=excluded.action`,
		name, valueBlob, allowJSON, action, time.Now().UnixNano())
	if err != nil {
		return fmt.Errorf("save secret: %w", err)
	}
	return nil
}

func (s *Store) SecretRecords(ctx context.Context) ([]SecretRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, value_blob, allow_json, action, created_at
		FROM secrets ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("query secrets: %w", err)
	}
	defer rows.Close()
	var records []SecretRecord
	for rows.Next() {
		var record SecretRecord
		var createdAt int64
		if err := rows.Scan(&record.Name, &record.ValueBlob, &record.AllowJSON, &record.Action, &createdAt); err != nil {
			return nil, fmt.Errorf("scan secret: %w", err)
		}
		record.CreatedAt = time.Unix(0, createdAt).UTC()
		record.ValueBlob = append([]byte(nil), record.ValueBlob...)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate secrets: %w", err)
	}
	return records, nil
}

func (s *Store) DeleteSecret(ctx context.Context, name string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE name = ?`, name); err != nil {
		return fmt.Errorf("delete secret: %w", err)
	}
	return nil
}

func (s *Store) CreateTree(ctx context.Context, tree TreeRecord, root BranchRecord) error {
	if tree.ID == "" || root.ID == "" || root.ID != tree.RootBranch || root.TreeID != tree.ID || root.ParentID != "" || root.Mode != "live" || root.Status != "active" || root.TokenHash == "" {
		return fmt.Errorf("create root branch: %w", ErrInvalidTransition)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tree creation: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO trees(tree_id, root_branch, created_at) VALUES (?, ?, ?)`, tree.ID, tree.RootBranch, tree.CreatedAt.UnixNano()); err != nil {
		return fmt.Errorf("insert tree: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO branches(branch_id, tree_id, parent_id, mode, status, token_hash, divergent_count, created_at) VALUES (?, ?, NULL, ?, ?, ?, ?, ?)`, root.ID, root.TreeID, root.Mode, root.Status, root.TokenHash, root.DivergentCount, root.CreatedAt.UnixNano()); err != nil {
		return fmt.Errorf("insert root branch: %w", err)
	}
	if err := recordEvent(ctx, tx, tree.ID, root.ID, "branch.created", map[string]any{"mode": root.Mode, "status": root.Status}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tree creation: %w", err)
	}
	return nil
}

func (s *Store) GetTree(ctx context.Context, id string) (TreeRecord, error) {
	var tree TreeRecord
	var createdAt int64
	if err := s.db.QueryRowContext(ctx, `SELECT tree_id, root_branch, created_at FROM trees WHERE tree_id = ?`, id).Scan(&tree.ID, &tree.RootBranch, &createdAt); err != nil {
		return TreeRecord{}, err
	}
	tree.CreatedAt = time.Unix(0, createdAt).UTC()
	return tree, nil
}

func (s *Store) GetBranch(ctx context.Context, id string) (BranchRecord, error) {
	return scanBranch(s.db.QueryRowContext(ctx, `SELECT branch_id, tree_id, COALESCE(parent_id, ''), mode, status, token_hash, divergent_count, created_at, sealed_at FROM branches WHERE branch_id = ?`, id))
}

func (s *Store) FindBranchByTokenHash(ctx context.Context, tokenHash string) (BranchRecord, error) {
	return scanBranch(s.db.QueryRowContext(ctx, `SELECT branch_id, tree_id, COALESCE(parent_id, ''), mode, status, token_hash, divergent_count, created_at, sealed_at FROM branches WHERE token_hash = ?`, tokenHash))
}

func (s *Store) ListBranches(ctx context.Context, treeID string) ([]BranchRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT branch_id, tree_id, COALESCE(parent_id, ''), mode, status, token_hash, divergent_count, created_at, sealed_at FROM branches WHERE tree_id = ? ORDER BY created_at, branch_id`, treeID)
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	defer rows.Close()
	var result []BranchRecord
	for rows.Next() {
		branch, err := scanBranchRow(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, branch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate branches: %w", err)
	}
	return result, nil
}

func (s *Store) ForkBranch(ctx context.Context, parentID string, children []BranchRecord) error {
	if parentID == "" || len(children) == 0 {
		return fmt.Errorf("branch fork requires a parent and children: %w", ErrInvalidTransition)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin branch fork: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE branches SET status = 'sealed', sealed_at = ? WHERE branch_id = ? AND status = 'active' AND mode = 'live' AND parent_id IS NULL`, time.Now().UnixNano(), parentID)
	if err != nil {
		return fmt.Errorf("seal parent branch: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("check sealed branch: %w", err)
	} else if count != 1 {
		return fmt.Errorf("branch cannot fork: %w", ErrInvalidTransition)
	}
	var parentTree string
	if err := tx.QueryRowContext(ctx, `SELECT tree_id FROM branches WHERE branch_id = ?`, parentID).Scan(&parentTree); err != nil {
		return fmt.Errorf("read parent branch: %w", err)
	}
	childIDs := make([]string, 0, len(children))
	for _, child := range children {
		if child.ID == "" || child.TreeID != parentTree || child.ParentID != parentID || child.Mode != "speculative" || child.Status != "active" || child.TokenHash == "" {
			return fmt.Errorf("invalid child branch: %w", ErrInvalidTransition)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO branches(branch_id, tree_id, parent_id, mode, status, token_hash, divergent_count, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, child.ID, child.TreeID, child.ParentID, child.Mode, child.Status, child.TokenHash, child.DivergentCount, child.CreatedAt.UnixNano()); err != nil {
			return fmt.Errorf("insert child branch: %w", err)
		}
		childIDs = append(childIDs, child.ID)
		if err := recordEvent(ctx, tx, parentTree, child.ID, "branch.created", map[string]any{"parent_id": parentID, "mode": child.Mode, "status": child.Status}); err != nil {
			return err
		}
	}
	if err := recordEvent(ctx, tx, parentTree, parentID, "branch.sealed", map[string]any{"status": "sealed"}); err != nil {
		return err
	}
	if err := recordEvent(ctx, tx, parentTree, parentID, "branch.forked", map[string]any{"children": childIDs, "count": len(childIDs)}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit branch fork: %w", err)
	}
	return nil
}

func (s *Store) AbortBranch(ctx context.Context, branchID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin branch abort: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE branches SET status = 'aborted' WHERE branch_id = ? AND status = 'active'`, branchID)
	if err != nil {
		return fmt.Errorf("abort branch: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("check aborted branch: %w", err)
	} else if count != 1 {
		return fmt.Errorf("branch cannot abort: %w", ErrInvalidTransition)
	}
	var treeID string
	if err := tx.QueryRowContext(ctx, `SELECT tree_id FROM branches WHERE branch_id = ?`, branchID).Scan(&treeID); err != nil {
		return fmt.Errorf("read aborted branch: %w", err)
	}
	discarded, err := tx.ExecContext(ctx, `UPDATE staged_writes SET status = 'discarded' WHERE branch_id = ? AND status = 'staged'`, branchID)
	if err != nil {
		return fmt.Errorf("discard staged writes: %w", err)
	}
	discardedCount, err := discarded.RowsAffected()
	if err != nil {
		return fmt.Errorf("count discarded writes: %w", err)
	}
	if discardedCount > 0 {
		if err := recordEvent(ctx, tx, treeID, branchID, "write.discarded", map[string]any{"count": discardedCount}); err != nil {
			return err
		}
	}
	if err := recordEvent(ctx, tx, treeID, branchID, "branch.aborted", map[string]any{"discarded_count": discardedCount}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit branch abort: %w", err)
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanBranch(row rowScanner) (BranchRecord, error) {
	var branch BranchRecord
	var createdAt int64
	var sealedAt sql.NullInt64
	if err := row.Scan(&branch.ID, &branch.TreeID, &branch.ParentID, &branch.Mode, &branch.Status, &branch.TokenHash, &branch.DivergentCount, &createdAt, &sealedAt); err != nil {
		return BranchRecord{}, err
	}
	branch.CreatedAt = time.Unix(0, createdAt).UTC()
	if sealedAt.Valid {
		value := time.Unix(0, sealedAt.Int64).UTC()
		branch.SealedAt = &value
	}
	return branch, nil
}

func scanBranchRow(rows *sql.Rows) (BranchRecord, error) {
	return scanBranch(rows)
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
