package db

import (
	"database/sql"
	"errors"
)

const metricTransferredBytes = "transferred_bytes"

func (db *DB) AddTransferredBytes(n int64) error {
	if n <= 0 {
		return nil
	}
	_, err := db.SQL.Exec(`
		INSERT INTO server_metrics (key, value)
		VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = value + excluded.value
	`, metricTransferredBytes, n)
	return err
}

func (db *DB) TransferredBytes() (int64, error) {
	var value int64
	err := db.SQL.QueryRow(`
		SELECT value
		FROM server_metrics
		WHERE key = ?
	`, metricTransferredBytes).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return value, nil
}
