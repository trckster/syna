package db

import (
	"database/sql"
	"time"
)

func incrementObjectRef(exec interface {
	Exec(query string, args ...any) (sql.Result, error)
}, objectID string) error {
	_, err := exec.Exec(`UPDATE objects SET ref_count = ref_count + 1, zero_ref_at = NULL WHERE object_id = ?`, objectID)
	return err
}

func decrementObjectRef(exec interface {
	Exec(query string, args ...any) (sql.Result, error)
}, objectID string, now time.Time) error {
	_, err := exec.Exec(`
		UPDATE objects
		SET
			ref_count = CASE WHEN ref_count > 0 THEN ref_count - 1 ELSE 0 END,
			zero_ref_at = CASE WHEN ref_count <= 1 THEN ? ELSE zero_ref_at END
		WHERE object_id = ?
	`, now, objectID)
	return err
}
