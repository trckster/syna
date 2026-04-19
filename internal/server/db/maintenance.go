package db

import (
	"time"
)

func (db *DB) Counts() (map[string]int64, error) {
	out := map[string]int64{}
	for name, query := range map[string]string{
		"workspaces": `SELECT COUNT(*) FROM workspaces`,
		"devices":    `SELECT COUNT(*) FROM devices`,
		"roots":      `SELECT COUNT(*) FROM roots`,
		"events":     `SELECT COUNT(*) FROM events`,
		"objects":    `SELECT COUNT(*) FROM objects`,
		"sessions":   `SELECT COUNT(*) FROM sessions`,
	} {
		var count int64
		if err := db.SQL.QueryRow(query).Scan(&count); err != nil {
			return nil, err
		}
		out[name] = count
	}
	return out, nil
}

func (db *DB) Prune(now time.Time, eventRetention, zeroRefRetention time.Duration) (int64, int64, []string, error) {
	var deletedEvents int64
	var deletedSnapshots int64

	rows, err := db.SQL.Query(`SELECT workspace_id, retained_floor_seq FROM workspaces`)
	if err != nil {
		return 0, 0, nil, err
	}
	defer rows.Close()
	type floor struct {
		workspaceID string
		seq         int64
	}
	var floors []floor
	for rows.Next() {
		var f floor
		if err := rows.Scan(&f.workspaceID, &f.seq); err != nil {
			return 0, 0, nil, err
		}
		floors = append(floors, f)
	}
	for _, f := range floors {
		cutoff := now.Add(-eventRetention)
		evRows, err := db.SQL.Query(`
			SELECT seq FROM events
			WHERE workspace_id = ? AND seq <= ? AND created_at <= ?
		`, f.workspaceID, f.seq, cutoff)
		if err != nil {
			return 0, 0, nil, err
		}
		var seqs []int64
		for evRows.Next() {
			var seq int64
			if err := evRows.Scan(&seq); err != nil {
				evRows.Close()
				return 0, 0, nil, err
			}
			seqs = append(seqs, seq)
		}
		evRows.Close()
		for _, seq := range seqs {
			refs, err := db.eventRefs(seq)
			if err != nil {
				return 0, 0, nil, err
			}
			for _, objectID := range refs {
				if err := decrementObjectRef(db.SQL, objectID, now); err != nil {
					return 0, 0, nil, err
				}
			}
			if _, err := db.SQL.Exec(`DELETE FROM event_object_refs WHERE event_seq = ?`, seq); err != nil {
				return 0, 0, nil, err
			}
			if _, err := db.SQL.Exec(`DELETE FROM events WHERE seq = ?`, seq); err != nil {
				return 0, 0, nil, err
			}
			deletedEvents++
		}
	}

	snapshotRows, err := db.SQL.Query(`
		SELECT s.workspace_id, s.root_id, s.object_id
		FROM snapshots s
		LEFT JOIN roots r ON r.workspace_id = s.workspace_id AND r.root_id = s.root_id
		WHERE s.created_at <= ?
		  AND (
			r.root_id IS NULL OR
			r.removed_seq IS NOT NULL OR
			(r.removed_seq IS NULL AND r.latest_snapshot_object_id IS NOT NULL AND s.object_id != r.latest_snapshot_object_id)
		  )
	`, now.Add(-eventRetention))
	if err != nil {
		return 0, 0, nil, err
	}
	var snapshotsToDelete [][3]string
	for snapshotRows.Next() {
		var row [3]string
		if err := snapshotRows.Scan(&row[0], &row[1], &row[2]); err != nil {
			snapshotRows.Close()
			return 0, 0, nil, err
		}
		snapshotsToDelete = append(snapshotsToDelete, row)
	}
	snapshotRows.Close()
	for _, row := range snapshotsToDelete {
		refRows, err := db.SQL.Query(`SELECT object_id FROM snapshot_object_refs WHERE snapshot_object_id = ?`, row[2])
		if err != nil {
			return 0, 0, nil, err
		}
		var refs []string
		for refRows.Next() {
			var objectID string
			if err := refRows.Scan(&objectID); err != nil {
				refRows.Close()
				return 0, 0, nil, err
			}
			refs = append(refs, objectID)
		}
		refRows.Close()
		if err := decrementObjectRef(db.SQL, row[2], now); err != nil {
			return 0, 0, nil, err
		}
		for _, objectID := range refs {
			if err := decrementObjectRef(db.SQL, objectID, now); err != nil {
				return 0, 0, nil, err
			}
		}
		if _, err := db.SQL.Exec(`DELETE FROM snapshot_object_refs WHERE snapshot_object_id = ?`, row[2]); err != nil {
			return 0, 0, nil, err
		}
		if _, err := db.SQL.Exec(`DELETE FROM snapshots WHERE workspace_id = ? AND root_id = ? AND object_id = ?`, row[0], row[1], row[2]); err != nil {
			return 0, 0, nil, err
		}
		deletedSnapshots++
	}

	objRows, err := db.SQL.Query(`SELECT object_id FROM objects WHERE ref_count = 0 AND COALESCE(zero_ref_at, created_at) <= ?`, now.Add(-zeroRefRetention))
	if err != nil {
		return 0, 0, nil, err
	}
	var objectIDs []string
	for objRows.Next() {
		var objectID string
		if err := objRows.Scan(&objectID); err != nil {
			objRows.Close()
			return 0, 0, nil, err
		}
		objectIDs = append(objectIDs, objectID)
	}
	objRows.Close()
	for _, objectID := range objectIDs {
		if _, err := db.SQL.Exec(`DELETE FROM objects WHERE object_id = ?`, objectID); err != nil {
			return 0, 0, nil, err
		}
	}
	return deletedEvents, deletedSnapshots, objectIDs, nil
}
