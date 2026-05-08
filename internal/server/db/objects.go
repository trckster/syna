package db

import (
	"database/sql"
	"fmt"
	"time"
)

func (db *DB) AssociateObjectWithWorkspace(workspaceID, objectID string) error {
	return associateObjectWithWorkspace(db.SQL, workspaceID, objectID, time.Now().UTC())
}

func (db *DB) ObjectVisibleToWorkspace(workspaceID, objectID string) bool {
	return objectVisibleToWorkspace(db.SQL, workspaceID, objectID)
}

func associateObjectWithWorkspace(exec interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}, workspaceID, objectID string, now time.Time) error {
	res, err := exec.Exec(`
		INSERT OR IGNORE INTO workspace_objects (workspace_id, object_id, created_at)
		SELECT ?, ?, ?
		WHERE EXISTS (SELECT 1 FROM workspaces WHERE workspace_id = ?)
		  AND EXISTS (SELECT 1 FROM objects WHERE object_id = ?)
	`, workspaceID, objectID, now, workspaceID, objectID)
	if err != nil {
		return err
	}
	affected, rowsErr := res.RowsAffected()
	if rowsErr != nil || affected == 0 {
		if !objectVisibleToWorkspace(exec, workspaceID, objectID) {
			return fmt.Errorf("object is not available for workspace")
		}
	}
	return nil
}

func objectVisibleToWorkspace(q interface {
	QueryRow(query string, args ...any) *sql.Row
}, workspaceID, objectID string) bool {
	var exists int
	return q.QueryRow(`
		SELECT 1
		FROM workspace_objects
		WHERE workspace_id = ? AND object_id = ?
	`, workspaceID, objectID).Scan(&exists) == nil
}
