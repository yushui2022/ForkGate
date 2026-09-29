package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// StagedWrite exposes only metadata. RequestBlob must already be encrypted by
// the caller, and is never included in JSON or ListStagedWrites results.
type StagedWrite struct {
	ID          string    `json:"id"`
	BranchID    string    `json:"branch_id"`
	Seq         int64     `json:"seq"`
	Method      string    `json:"method"`
	Host        string    `json:"host"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	RequestBlob []byte    `json:"-"`
}

func (s *Store) migrateStaging() error {
	const schema = `
CREATE TABLE IF NOT EXISTS staged_writes (
  staged_id    TEXT PRIMARY KEY,
  branch_id    TEXT NOT NULL REFERENCES branches(branch_id),
  seq          INTEGER NOT NULL CHECK (seq > 0),
  method       TEXT NOT NULL,
  host         TEXT NOT NULL,
  status       TEXT NOT NULL CHECK (status IN ('staged', 'sending', 'succeeded', 'failed', 'unknown', 'discarded')),
  request_blob BLOB NOT NULL,
  created_at   INTEGER NOT NULL,
  UNIQUE (branch_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_staged_writes_branch ON staged_writes(branch_id, seq);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate staged writes: %w", err)
	}
	return nil
}

// SaveStagedWrite atomically checks branch state, allocates the branch sequence,
// persists the ciphertext and records the event. Metadata is set after commit.
func (s *Store) SaveStagedWrite(ctx context.Context, write *StagedWrite) error {
	if write == nil || write.ID == "" || write.BranchID == "" || write.Method == "" || write.Host == "" || len(write.RequestBlob) == 0 {
		return errors.New("staged write requires identity, request metadata and encrypted payload")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin staged write: %w", err)
	}
	defer tx.Rollback()
	var treeID, mode, status string
	if err := tx.QueryRowContext(ctx, `SELECT tree_id, mode, status FROM branches WHERE branch_id = ?`, write.BranchID).Scan(&treeID, &mode, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("branch cannot stage writes: %w", ErrInvalidTransition)
		}
		return fmt.Errorf("read staging branch: %w", err)
	}
	if mode != "speculative" || status != "active" {
		return fmt.Errorf("branch cannot stage writes: %w", ErrInvalidTransition)
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM staged_writes WHERE branch_id = ?`, write.BranchID).Scan(&seq); err != nil {
		return fmt.Errorf("allocate staged sequence: %w", err)
	}
	createdAt := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO staged_writes(staged_id, branch_id, seq, method, host, status, request_blob, created_at) VALUES (?, ?, ?, ?, ?, 'staged', ?, ?)`, write.ID, write.BranchID, seq, write.Method, write.Host, write.RequestBlob, createdAt.UnixNano()); err != nil {
		return fmt.Errorf("insert staged write: %w", err)
	}
	if err := recordEvent(ctx, tx, treeID, write.BranchID, "write.staged", map[string]any{"staged_id": write.ID, "seq": seq, "method": write.Method, "host": write.Host}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit staged write: %w", err)
	}
	write.Seq = seq
	write.Status = "staged"
	write.CreatedAt = createdAt
	return nil
}

// ListStagedWrites returns ordered, public metadata without loading ciphertext.
func (s *Store) ListStagedWrites(ctx context.Context, branchID string) ([]StagedWrite, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT staged_id, branch_id, seq, method, host, status, created_at FROM staged_writes WHERE branch_id = ? ORDER BY seq`, branchID)
	if err != nil {
		return nil, fmt.Errorf("list staged writes: %w", err)
	}
	defer rows.Close()
	writes := make([]StagedWrite, 0)
	for rows.Next() {
		var write StagedWrite
		var createdAt int64
		if err := rows.Scan(&write.ID, &write.BranchID, &write.Seq, &write.Method, &write.Host, &write.Status, &createdAt); err != nil {
			return nil, fmt.Errorf("scan staged write: %w", err)
		}
		write.CreatedAt = time.Unix(0, createdAt).UTC()
		writes = append(writes, write)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate staged writes: %w", err)
	}
	return writes, nil
}

// LoadStagedWrites returns ordered staged metadata together with the encrypted
// request payload. It is intentionally separate from ListStagedWrites so the
// ciphertext never leaks into control-plane responses.
func (s *Store) LoadStagedWrites(ctx context.Context, branchID string) ([]StagedWrite, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT staged_id, branch_id, seq, method, host, status, request_blob, created_at FROM staged_writes WHERE branch_id = ? ORDER BY seq`, branchID)
	if err != nil {
		return nil, fmt.Errorf("load staged writes: %w", err)
	}
	defer rows.Close()
	writes := make([]StagedWrite, 0)
	for rows.Next() {
		var write StagedWrite
		var createdAt int64
		if err := rows.Scan(&write.ID, &write.BranchID, &write.Seq, &write.Method, &write.Host, &write.Status, &write.RequestBlob, &createdAt); err != nil {
			return nil, fmt.Errorf("scan loaded staged write: %w", err)
		}
		write.CreatedAt = time.Unix(0, createdAt).UTC()
		write.RequestBlob = append([]byte(nil), write.RequestBlob...)
		writes = append(writes, write)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate loaded staged writes: %w", err)
	}
	return writes, nil
}

// SetStagedStatus updates one write and records the transition. The caller
// owns serialization of commit operations at the branch manager layer.
func (s *Store) SetStagedStatus(ctx context.Context, stagedID, status string, responseStatus int) error {
	if stagedID == "" {
		return errors.New("staged id is required")
	}
	switch status {
	case "sending", "succeeded", "failed", "unknown":
	default:
		return fmt.Errorf("invalid staged status %q", status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin staged status: %w", err)
	}
	defer tx.Rollback()
	var treeID, branchID string
	if err := tx.QueryRowContext(ctx, `SELECT b.tree_id, w.branch_id FROM staged_writes w JOIN branches b ON b.branch_id = w.branch_id WHERE w.staged_id = ?`, stagedID).Scan(&treeID, &branchID); err != nil {
		return fmt.Errorf("find staged write: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE staged_writes SET status = ? WHERE staged_id = ?`, status, stagedID)
	if err != nil {
		return fmt.Errorf("update staged status: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return fmt.Errorf("check staged status: %w", err)
		}
		return fmt.Errorf("staged write not found: %w", sql.ErrNoRows)
	}
	payload := map[string]any{"staged_id": stagedID, "status": status}
	if responseStatus != 0 {
		payload["response_status"] = responseStatus
	}
	if err := recordEvent(ctx, tx, treeID, branchID, "write."+status, payload); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit staged status: %w", err)
	}
	return nil
}

// CommitBranch marks the selected speculative branch committed and discards
// active sibling branches in the same fork. It is one transaction so control
// plane readers never observe a half-resolved fork.
func (s *Store) CommitBranch(ctx context.Context, branchID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin branch commit: %w", err)
	}
	defer tx.Rollback()
	var treeID, parentID, mode, status string
	if err := tx.QueryRowContext(ctx, `SELECT tree_id, COALESCE(parent_id, ''), mode, status FROM branches WHERE branch_id = ?`, branchID).Scan(&treeID, &parentID, &mode, &status); err != nil {
		return err
	}
	if mode != "speculative" || status != "active" || parentID == "" {
		return fmt.Errorf("branch cannot commit: %w", ErrInvalidTransition)
	}
	result, err := tx.ExecContext(ctx, `UPDATE branches SET status = 'committed' WHERE branch_id = ? AND status = 'active'`, branchID)
	if err != nil {
		return fmt.Errorf("commit branch: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return fmt.Errorf("check committed branch: %w", err)
		}
		return fmt.Errorf("branch cannot commit: %w", ErrInvalidTransition)
	}
	if err := recordEvent(ctx, tx, treeID, branchID, "branch.committed", map[string]any{"status": "committed"}); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT branch_id FROM branches WHERE parent_id = ? AND branch_id <> ? AND status = 'active'`, parentID, branchID)
	if err != nil {
		return fmt.Errorf("list sibling branches: %w", err)
	}
	var siblings []string
	for rows.Next() {
		var siblingID string
		if err := rows.Scan(&siblingID); err != nil {
			rows.Close()
			return fmt.Errorf("scan sibling branch: %w", err)
		}
		siblings = append(siblings, siblingID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate sibling branches: %w", err)
	}
	rows.Close()
	for _, siblingID := range siblings {
		if _, err := tx.ExecContext(ctx, `UPDATE branches SET status = 'aborted' WHERE branch_id = ? AND status = 'active'`, siblingID); err != nil {
			return fmt.Errorf("abort sibling branch: %w", err)
		}
		var discarded int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM staged_writes WHERE branch_id = ? AND status IN ('staged', 'sending')`, siblingID).Scan(&discarded); err != nil {
			return fmt.Errorf("count sibling writes: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE staged_writes SET status = 'discarded' WHERE branch_id = ? AND status IN ('staged', 'sending')`, siblingID); err != nil {
			return fmt.Errorf("discard sibling writes: %w", err)
		}
		if discarded > 0 {
			if err := recordEvent(ctx, tx, treeID, siblingID, "write.discarded", map[string]any{"count": discarded}); err != nil {
				return err
			}
		}
		if err := recordEvent(ctx, tx, treeID, siblingID, "branch.aborted", map[string]any{"discarded_count": discarded, "reason": "sibling_committed"}); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit branch state: %w", err)
	}
	return nil
}
