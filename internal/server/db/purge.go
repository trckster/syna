package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrWorkspaceNotFound = errors.New("workspace not found")

type PurgeObject struct {
	ObjectID  string
	SizeBytes int64
}

type PurgeResult struct {
	Devices   int64
	Sessions  int64
	Roots     int64
	Events    int64
	Snapshots int64
	Objects   []PurgeObject
}

func (db *DB) WorkspacePurgeObjects(workspaceID string) ([]PurgeObject, error) {
	rows, err := db.SQL.Query(`
		SELECT wo.object_id, o.size_bytes
		FROM workspace_objects wo
		JOIN objects o ON o.object_id = wo.object_id
		WHERE wo.workspace_id = ?
		  AND NOT EXISTS (
			SELECT 1 FROM workspace_objects other
			WHERE other.object_id = wo.object_id AND other.workspace_id != ?
		  )
		ORDER BY wo.object_id
	`, workspaceID, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var objects []PurgeObject
	for rows.Next() {
		var object PurgeObject
		if err := rows.Scan(&object.ObjectID, &object.SizeBytes); err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	return objects, rows.Err()
}

func (db *DB) PurgeWorkspace(workspaceID string, candidates []PurgeObject, now time.Time) (PurgeResult, error) {
	if _, err := db.SQL.Exec(`PRAGMA secure_delete = ON`); err != nil {
		return PurgeResult{}, err
	}
	tx, err := db.Begin(context.Background())
	if err != nil {
		return PurgeResult{}, err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM workspaces WHERE workspace_id = ?`, workspaceID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PurgeResult{}, ErrWorkspaceNotFound
		}
		return PurgeResult{}, err
	}

	result := PurgeResult{}
	if result.Sessions, err = deleteCount(tx, `DELETE FROM sessions WHERE workspace_id = ?`, workspaceID); err != nil {
		return PurgeResult{}, err
	}
	if _, err := tx.Exec(`DELETE FROM session_challenges WHERE workspace_id = ?`, workspaceID); err != nil {
		return PurgeResult{}, err
	}
	if result.Devices, err = deleteCount(tx, `DELETE FROM devices WHERE workspace_id = ?`, workspaceID); err != nil {
		return PurgeResult{}, err
	}
	if _, err := tx.Exec(`DELETE FROM event_object_refs WHERE event_seq IN (SELECT seq FROM events WHERE workspace_id = ?)`, workspaceID); err != nil {
		return PurgeResult{}, err
	}
	if result.Events, err = deleteCount(tx, `DELETE FROM events WHERE workspace_id = ?`, workspaceID); err != nil {
		return PurgeResult{}, err
	}
	if _, err := tx.Exec(`
		DELETE FROM snapshot_object_refs
		WHERE snapshot_object_id IN (
			SELECT target.object_id
			FROM snapshots target
			WHERE target.workspace_id = ?
			  AND NOT EXISTS (
				SELECT 1 FROM snapshots other
				WHERE other.object_id = target.object_id AND other.workspace_id != ?
			  )
		)
	`, workspaceID, workspaceID); err != nil {
		return PurgeResult{}, err
	}
	if result.Snapshots, err = deleteCount(tx, `DELETE FROM snapshots WHERE workspace_id = ?`, workspaceID); err != nil {
		return PurgeResult{}, err
	}
	if _, err := tx.Exec(`DELETE FROM path_heads WHERE workspace_id = ?`, workspaceID); err != nil {
		return PurgeResult{}, err
	}
	if result.Roots, err = deleteCount(tx, `DELETE FROM roots WHERE workspace_id = ?`, workspaceID); err != nil {
		return PurgeResult{}, err
	}
	if _, err := tx.Exec(`DELETE FROM workspace_objects WHERE workspace_id = ?`, workspaceID); err != nil {
		return PurgeResult{}, err
	}
	if _, err := tx.Exec(`DELETE FROM workspaces WHERE workspace_id = ?`, workspaceID); err != nil {
		return PurgeResult{}, err
	}

	if _, err := tx.Exec(`
		UPDATE objects
		SET ref_count =
			(SELECT COUNT(*) FROM event_object_refs er WHERE er.object_id = objects.object_id) +
			(SELECT COUNT(*) FROM snapshots s WHERE s.object_id = objects.object_id) +
			(SELECT COUNT(*) FROM snapshot_object_refs sr WHERE sr.object_id = objects.object_id),
			zero_ref_at = CASE WHEN
				(SELECT COUNT(*) FROM event_object_refs er WHERE er.object_id = objects.object_id) +
				(SELECT COUNT(*) FROM snapshots s WHERE s.object_id = objects.object_id) +
				(SELECT COUNT(*) FROM snapshot_object_refs sr WHERE sr.object_id = objects.object_id) = 0
			THEN COALESCE(zero_ref_at, ?) ELSE NULL END
	`, now); err != nil {
		return PurgeResult{}, err
	}

	for _, object := range candidates {
		res, err := tx.Exec(`
			DELETE FROM objects
			WHERE object_id = ?
			  AND NOT EXISTS (SELECT 1 FROM workspace_objects WHERE object_id = ?)
			  AND NOT EXISTS (SELECT 1 FROM event_object_refs WHERE object_id = ?)
			  AND NOT EXISTS (SELECT 1 FROM snapshots WHERE object_id = ?)
			  AND NOT EXISTS (SELECT 1 FROM snapshot_object_refs WHERE object_id = ?)
		`, object.ObjectID, object.ObjectID, object.ObjectID, object.ObjectID, object.ObjectID)
		if err != nil {
			return PurgeResult{}, err
		}
		if affected, _ := res.RowsAffected(); affected > 0 {
			result.Objects = append(result.Objects, object)
		}
	}
	if err := tx.Commit(); err != nil {
		return PurgeResult{}, err
	}
	return result, nil
}

func (db *DB) CompactAfterPurge() error {
	if _, err := db.SQL.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("truncate SQLite WAL: %w", err)
	}
	if _, err := db.SQL.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("vacuum SQLite database: %w", err)
	}
	return nil
}

func deleteCount(tx *sql.Tx, query string, args ...any) (int64, error) {
	result, err := tx.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
