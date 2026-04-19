package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"syna/internal/common/protocol"
)

func (db *DB) SaveSnapshot(sess *Session, req protocol.SnapshotSubmitRequest) error {
	tx, err := db.Begin(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var removedSeq sql.NullInt64
	err = tx.QueryRow(`
		SELECT removed_seq
		FROM roots
		WHERE workspace_id = ? AND root_id = ?
	`, sess.WorkspaceID, req.RootID).Scan(&removedSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("unknown root")
	}
	if err != nil {
		return err
	}
	if removedSeq.Valid {
		return fmt.Errorf("root removed")
	}
	if !objectExists(tx, req.ObjectID) {
		return fmt.Errorf("missing snapshot object")
	}
	for _, objectID := range req.ObjectRefs {
		if !objectExists(tx, objectID) {
			return fmt.Errorf("missing snapshot object ref %s", objectID)
		}
	}

	now := time.Now().UTC()
	insertRes, err := tx.Exec(`
		INSERT OR IGNORE INTO snapshots (workspace_id, root_id, object_id, base_seq, author_device_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, sess.WorkspaceID, req.RootID, req.ObjectID, req.BaseSeq, sess.DeviceID, now)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE roots
		SET latest_snapshot_object_id = ?, latest_snapshot_seq = ?, updated_at = ?
		WHERE workspace_id = ? AND root_id = ?
	`, req.ObjectID, req.BaseSeq, now, sess.WorkspaceID, req.RootID); err != nil {
		return err
	}
	if affected, _ := insertRes.RowsAffected(); affected > 0 {
		if err := incrementObjectRef(tx, req.ObjectID); err != nil {
			return err
		}
	}
	for _, objectID := range req.ObjectRefs {
		refRes, err := tx.Exec(`INSERT OR IGNORE INTO snapshot_object_refs (snapshot_object_id, object_id) VALUES (?, ?)`, req.ObjectID, objectID)
		if err != nil {
			return err
		}
		if affected, _ := refRes.RowsAffected(); affected > 0 {
			if err := incrementObjectRef(tx, objectID); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.RecomputeRetainedFloor(sess.WorkspaceID)
}

func objectExists(q interface {
	QueryRow(query string, args ...any) *sql.Row
}, objectID string) bool {
	var exists int
	return q.QueryRow(`SELECT 1 FROM objects WHERE object_id = ?`, objectID).Scan(&exists) == nil
}
