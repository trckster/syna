package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrWorkspaceLimitReached = errors.New("workspace limit reached")

func (db *DB) EnsureWorkspace(workspaceID string, pubKey []byte) (bool, error) {
	return db.EnsureWorkspaceWithinLimit(workspaceID, pubKey, 0)
}

func (db *DB) EnsureWorkspaceWithinLimit(workspaceID string, pubKey []byte, maxWorkspaces int) (bool, error) {
	now := time.Now().UTC()
	tx, err := db.Begin(context.Background())
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var existing []byte
	err = tx.QueryRow(`SELECT auth_pubkey FROM workspaces WHERE workspace_id = ?`, workspaceID).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if len(pubKey) == 0 {
			return false, sql.ErrNoRows
		}
		if maxWorkspaces > 0 {
			var count int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM workspaces`).Scan(&count); err != nil {
				return false, err
			}
			if count >= maxWorkspaces {
				return false, ErrWorkspaceLimitReached
			}
		}
		if _, err := tx.Exec(`
			INSERT INTO workspaces (workspace_id, auth_pubkey, current_seq, retained_floor_seq, created_at, updated_at)
			VALUES (?, ?, 0, 0, ?, ?)
		`, workspaceID, pubKey, now, now); err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return true, nil
	case err != nil:
		return false, err
	default:
		if len(pubKey) > 0 && string(pubKey) != string(existing) {
			return false, fmt.Errorf("workspace public key mismatch")
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
}

func (db *DB) WorkspacePubKey(workspaceID string) ([]byte, error) {
	var pubKey []byte
	err := db.SQL.QueryRow(`SELECT auth_pubkey FROM workspaces WHERE workspace_id = ?`, workspaceID).Scan(&pubKey)
	return pubKey, err
}

func (db *DB) CurrentSeq(workspaceID string) (int64, error) {
	var seq int64
	err := db.SQL.QueryRow(`SELECT current_seq FROM workspaces WHERE workspace_id = ?`, workspaceID).Scan(&seq)
	return seq, err
}

func (db *DB) RetainedFloor(workspaceID string) (int64, error) {
	var seq int64
	err := db.SQL.QueryRow(`SELECT retained_floor_seq FROM workspaces WHERE workspace_id = ?`, workspaceID).Scan(&seq)
	return seq, err
}

func (db *DB) ActiveRoots(workspaceID string) ([]RootInfo, error) {
	rows, err := db.SQL.Query(`
		SELECT root_id, kind, descriptor_blob, created_seq, removed_seq, latest_snapshot_object_id, latest_snapshot_seq
		FROM roots
		WHERE workspace_id = ? AND removed_seq IS NULL
		ORDER BY root_id ASC
	`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roots []RootInfo
	for rows.Next() {
		var root RootInfo
		if err := rows.Scan(
			&root.RootID,
			&root.Kind,
			&root.DescriptorBlob,
			&root.CreatedSeq,
			&root.RemovedSeq,
			&root.LatestSnapshotObjectID,
			&root.LatestSnapshotSeq,
		); err != nil {
			return nil, err
		}
		roots = append(roots, root)
	}
	return roots, rows.Err()
}

func (db *DB) RecomputeRetainedFloor(workspaceID string) error {
	roots, err := db.ActiveRoots(workspaceID)
	if err != nil {
		return err
	}
	currentSeq, err := db.CurrentSeq(workspaceID)
	if err != nil {
		return err
	}
	var floor int64
	switch {
	case len(roots) == 0:
		floor = currentSeq
	default:
		floor = 0
		for _, root := range roots {
			if !root.LatestSnapshotSeq.Valid {
				floor = 0
				break
			}
			if root.LatestSnapshotSeq.Int64 < floor || floor == 0 {
				floor = root.LatestSnapshotSeq.Int64
			}
		}
	}
	_, err = db.SQL.Exec(`UPDATE workspaces SET retained_floor_seq = ?, updated_at = ? WHERE workspace_id = ?`, floor, time.Now().UTC(), workspaceID)
	return err
}
