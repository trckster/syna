package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"syna/internal/common/protocol"
)

type SubmitResult struct {
	AcceptedSeq  int64
	WorkspaceSeq int64
	Event        protocol.EventRecord
}

func (db *DB) SubmitEvent(sess *Session, req protocol.EventSubmitRequest) (*SubmitResult, error) {
	if err := validateEventRequest(req); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tx, err := db.Begin(context.Background())
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var currentWorkspaceSeq int64
	if err := tx.QueryRow(`SELECT current_seq FROM workspaces WHERE workspace_id = ?`, sess.WorkspaceID).Scan(&currentWorkspaceSeq); err != nil {
		return nil, err
	}
	for _, objectID := range req.ObjectRefs {
		var exists int
		if err := tx.QueryRow(`SELECT 1 FROM objects WHERE object_id = ?`, objectID).Scan(&exists); err != nil {
			return nil, fmt.Errorf("missing object ref %s", objectID)
		}
	}

	switch req.EventType {
	case protocol.EventRootAdd:
		if req.RootKind != protocol.RootKindDir && req.RootKind != protocol.RootKindFile {
			return nil, fmt.Errorf("root_add requires root_kind")
		}
		var removedSeq sql.NullInt64
		err := tx.QueryRow(`
			SELECT removed_seq FROM roots
			WHERE workspace_id = ? AND root_id = ?
		`, sess.WorkspaceID, req.RootID).Scan(&removedSeq)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil && !removedSeq.Valid {
			return nil, fmt.Errorf("root already active")
		}
	case protocol.EventRootRemove:
		var removedSeq sql.NullInt64
		err := tx.QueryRow(`
			SELECT removed_seq FROM roots
			WHERE workspace_id = ? AND root_id = ?
		`, sess.WorkspaceID, req.RootID).Scan(&removedSeq)
		if err != nil {
			return nil, fmt.Errorf("unknown root")
		}
		if removedSeq.Valid {
			return nil, fmt.Errorf("root already removed")
		}
	default:
		if req.PathID == "" || req.BaseSeq == nil {
			return nil, fmt.Errorf("content events require path_id and base_seq")
		}
		var removedSeq sql.NullInt64
		err := tx.QueryRow(`
			SELECT removed_seq FROM roots
			WHERE workspace_id = ? AND root_id = ?
		`, sess.WorkspaceID, req.RootID).Scan(&removedSeq)
		if err != nil {
			return nil, fmt.Errorf("unknown root")
		}
		if removedSeq.Valid {
			return nil, fmt.Errorf("root removed")
		}

		var headSeq int64
		err = tx.QueryRow(`
			SELECT current_seq FROM path_heads
			WHERE workspace_id = ? AND root_id = ? AND path_id = ?
		`, sess.WorkspaceID, req.RootID, req.PathID).Scan(&headSeq)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if *req.BaseSeq != 0 {
				return nil, &PathHeadMismatchError{CurrentSeq: 0}
			}
		case err != nil:
			return nil, err
		default:
			if headSeq != *req.BaseSeq {
				return nil, &PathHeadMismatchError{CurrentSeq: headSeq}
			}
		}
	}

	res, err := tx.Exec(`
		INSERT INTO events (workspace_id, root_id, path_id, event_type, base_seq, author_device_id, payload_blob, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, sess.WorkspaceID, req.RootID, nullString(req.PathID), string(req.EventType), nullInt(req.BaseSeq), sess.DeviceID, []byte(req.PayloadBlob), now)
	if err != nil {
		return nil, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	switch req.EventType {
	case protocol.EventRootAdd:
		if _, err := tx.Exec(`
			INSERT INTO roots (workspace_id, root_id, kind, descriptor_blob, created_seq, removed_seq, latest_snapshot_object_id, latest_snapshot_seq, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, NULL, NULL, NULL, ?, ?)
			ON CONFLICT(workspace_id, root_id) DO UPDATE SET
				kind = excluded.kind,
				descriptor_blob = excluded.descriptor_blob,
				created_seq = excluded.created_seq,
				removed_seq = NULL,
				latest_snapshot_object_id = NULL,
				latest_snapshot_seq = NULL,
				updated_at = excluded.updated_at
		`, sess.WorkspaceID, req.RootID, req.RootKind, []byte(req.PayloadBlob), seq, now, now); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM path_heads WHERE workspace_id = ? AND root_id = ?`, sess.WorkspaceID, req.RootID); err != nil {
			return nil, err
		}
	case protocol.EventRootRemove:
		if _, err := tx.Exec(`
			UPDATE roots
			SET removed_seq = ?, latest_snapshot_object_id = NULL, latest_snapshot_seq = NULL, updated_at = ?
			WHERE workspace_id = ? AND root_id = ?
		`, seq, now, sess.WorkspaceID, req.RootID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM path_heads WHERE workspace_id = ? AND root_id = ?`, sess.WorkspaceID, req.RootID); err != nil {
			return nil, err
		}
	default:
		deleted := 0
		if req.EventType == protocol.EventDelete {
			deleted = 1
		}
		if _, err := tx.Exec(`
			INSERT INTO path_heads (workspace_id, root_id, path_id, entry_kind, current_seq, deleted, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(workspace_id, root_id, path_id) DO UPDATE SET
				entry_kind = excluded.entry_kind,
				current_seq = excluded.current_seq,
				deleted = excluded.deleted,
				updated_at = excluded.updated_at
		`, sess.WorkspaceID, req.RootID, req.PathID, entryKindFromEvent(req.EventType), seq, deleted, now); err != nil {
			return nil, err
		}
	}

	for _, objectID := range req.ObjectRefs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO event_object_refs (event_seq, object_id) VALUES (?, ?)`, seq, objectID); err != nil {
			return nil, err
		}
		if err := incrementObjectRef(tx, objectID); err != nil {
			return nil, err
		}
	}

	if _, err := tx.Exec(`UPDATE devices SET last_seen_at = ? WHERE workspace_id = ? AND device_id = ?`, now, sess.WorkspaceID, sess.DeviceID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE workspaces SET current_seq = ?, updated_at = ? WHERE workspace_id = ?`, seq, now, sess.WorkspaceID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if req.EventType == protocol.EventRootAdd || req.EventType == protocol.EventRootRemove {
		if err := db.RecomputeRetainedFloor(sess.WorkspaceID); err != nil {
			return nil, err
		}
	}

	event := protocol.EventRecord{
		Seq:            seq,
		RootID:         req.RootID,
		EventType:      req.EventType,
		AuthorDeviceID: sess.DeviceID,
		PayloadBlob:    req.PayloadBlob,
		ObjectRefs:     append([]string(nil), req.ObjectRefs...),
		CreatedAt:      now,
	}
	if req.PathID != "" {
		pathID := req.PathID
		event.PathID = &pathID
	}
	if req.BaseSeq != nil {
		baseSeq := *req.BaseSeq
		event.BaseSeq = &baseSeq
	}
	return &SubmitResult{
		AcceptedSeq:  seq,
		WorkspaceSeq: seq,
		Event:        event,
	}, nil
}

type PathHeadMismatchError struct {
	CurrentSeq int64
}

func (e *PathHeadMismatchError) Error() string {
	return "path head mismatch"
}

func (db *DB) FetchEvents(workspaceID string, afterSeq int64, limit int) ([]protocol.EventRecord, int64, error) {
	floor, err := db.RetainedFloor(workspaceID)
	if err != nil {
		return nil, 0, err
	}
	if afterSeq < floor {
		return nil, floor, &ResyncRequiredError{RetainedFloorSeq: floor}
	}
	rows, err := db.SQL.Query(`
		SELECT seq, root_id, path_id, event_type, base_seq, author_device_id, payload_blob, created_at
		FROM events
		WHERE workspace_id = ? AND seq > ?
		ORDER BY seq ASC
		LIMIT ?
	`, workspaceID, afterSeq, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var events []protocol.EventRecord
	for rows.Next() {
		var event protocol.EventRecord
		var pathID sql.NullString
		var baseSeq sql.NullInt64
		var payload []byte
		if err := rows.Scan(&event.Seq, &event.RootID, &pathID, &event.EventType, &baseSeq, &event.AuthorDeviceID, &payload, &event.CreatedAt); err != nil {
			return nil, 0, err
		}
		if pathID.Valid {
			event.PathID = &pathID.String
		}
		if baseSeq.Valid {
			event.BaseSeq = &baseSeq.Int64
		}
		event.PayloadBlob = string(payload)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	rows.Close()
	for i := range events {
		refs, err := db.eventRefs(events[i].Seq)
		if err != nil {
			return nil, 0, err
		}
		events[i].ObjectRefs = refs
	}
	currentSeq, err := db.CurrentSeq(workspaceID)
	if err != nil {
		return nil, 0, err
	}
	return events, currentSeq, nil
}

type ResyncRequiredError struct {
	RetainedFloorSeq int64
}

func (e *ResyncRequiredError) Error() string {
	return "resync required"
}

func (db *DB) eventRefs(seq int64) ([]string, error) {
	rows, err := db.SQL.Query(`SELECT object_id FROM event_object_refs WHERE event_seq = ? ORDER BY object_id ASC`, seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var objectID string
		if err := rows.Scan(&objectID); err != nil {
			return nil, err
		}
		refs = append(refs, objectID)
	}
	return refs, rows.Err()
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func entryKindFromEvent(t protocol.EventType) string {
	switch t {
	case protocol.EventDirPut:
		return "dir"
	case protocol.EventFilePut:
		return "file"
	default:
		return "delete"
	}
}

func validateEventRequest(req protocol.EventSubmitRequest) error {
	if req.RootID == "" {
		return fmt.Errorf("root_id is required")
	}
	switch req.EventType {
	case protocol.EventRootAdd, protocol.EventRootRemove, protocol.EventDirPut, protocol.EventFilePut, protocol.EventDelete:
		return nil
	default:
		return fmt.Errorf("unsupported event_type %q", req.EventType)
	}
}
