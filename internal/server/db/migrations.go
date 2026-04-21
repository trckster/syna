package db

import (
	"strings"
)

func (db *DB) Migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY);`,
		`CREATE TABLE IF NOT EXISTS workspaces (
			workspace_id TEXT PRIMARY KEY,
			auth_pubkey BLOB NOT NULL,
			current_seq INTEGER NOT NULL DEFAULT 0,
			retained_floor_seq INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS devices (
			workspace_id TEXT NOT NULL,
			device_id TEXT NOT NULL,
			display_name TEXT NOT NULL,
			first_seen_at TIMESTAMP NOT NULL,
			last_seen_at TIMESTAMP NOT NULL,
			PRIMARY KEY (workspace_id, device_id)
		);`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token_hash BLOB PRIMARY KEY,
			workspace_id TEXT NOT NULL,
			device_id TEXT NOT NULL,
			issued_at TIMESTAMP NOT NULL,
			expires_at TIMESTAMP NOT NULL,
			last_used_at TIMESTAMP NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS session_challenges (
			workspace_id TEXT NOT NULL,
			device_id TEXT NOT NULL,
			client_nonce BLOB NOT NULL,
			server_nonce BLOB NOT NULL,
			device_name TEXT NOT NULL DEFAULT '',
			expires_at TIMESTAMP NOT NULL,
			created_at TIMESTAMP NOT NULL,
			PRIMARY KEY (workspace_id, device_id, client_nonce)
		);`,
		`CREATE TABLE IF NOT EXISTS roots (
			workspace_id TEXT NOT NULL,
			root_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			descriptor_blob BLOB NOT NULL,
			created_seq INTEGER NOT NULL,
			removed_seq INTEGER NULL,
			latest_snapshot_object_id TEXT NULL,
			latest_snapshot_seq INTEGER NULL,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			PRIMARY KEY (workspace_id, root_id)
		);`,
		`CREATE TABLE IF NOT EXISTS path_heads (
			workspace_id TEXT NOT NULL,
			root_id TEXT NOT NULL,
			path_id TEXT NOT NULL,
			entry_kind TEXT NOT NULL,
			current_seq INTEGER NOT NULL,
			deleted INTEGER NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			PRIMARY KEY (workspace_id, root_id, path_id)
		);`,
		`CREATE TABLE IF NOT EXISTS events (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id TEXT NOT NULL,
			root_id TEXT NOT NULL,
			path_id TEXT NULL,
			event_type TEXT NOT NULL,
			base_seq INTEGER NULL,
			author_device_id TEXT NOT NULL,
			payload_blob BLOB NOT NULL,
			created_at TIMESTAMP NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS event_object_refs (
			event_seq INTEGER NOT NULL,
			object_id TEXT NOT NULL,
			PRIMARY KEY (event_seq, object_id)
		);`,
		`CREATE TABLE IF NOT EXISTS snapshots (
			workspace_id TEXT NOT NULL,
			root_id TEXT NOT NULL,
			object_id TEXT NOT NULL,
			base_seq INTEGER NOT NULL,
			author_device_id TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL,
			PRIMARY KEY (workspace_id, root_id, object_id)
		);`,
		`CREATE TABLE IF NOT EXISTS snapshot_object_refs (
			snapshot_object_id TEXT NOT NULL,
			object_id TEXT NOT NULL,
			PRIMARY KEY (snapshot_object_id, object_id)
		);`,
		`CREATE TABLE IF NOT EXISTS objects (
			object_id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			storage_rel_path TEXT NOT NULL,
			ref_count INTEGER NOT NULL,
			zero_ref_at TIMESTAMP NULL,
			created_at TIMESTAMP NOT NULL,
			last_accessed_at TIMESTAMP NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS server_metrics (
			key TEXT PRIMARY KEY,
			value INTEGER NOT NULL
		);`,
		`INSERT OR IGNORE INTO schema_migrations(version) VALUES (1);`,
	}
	for _, stmt := range stmts {
		if _, err := db.SQL.Exec(stmt); err != nil {
			return err
		}
	}
	if _, err := db.SQL.Exec(`ALTER TABLE objects ADD COLUMN zero_ref_at TIMESTAMP NULL`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	if _, err := db.SQL.Exec(`ALTER TABLE session_challenges ADD COLUMN device_name TEXT NOT NULL DEFAULT ''`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	if _, err := db.SQL.Exec(`CREATE TABLE IF NOT EXISTS server_metrics (key TEXT PRIMARY KEY, value INTEGER NOT NULL)`); err != nil {
		return err
	}
	if _, err := db.SQL.Exec(`INSERT OR IGNORE INTO schema_migrations(version) VALUES (?)`, LatestSchemaVersion); err != nil {
		return err
	}
	return nil
}

func (db *DB) CurrentSchemaVersion() (int, error) {
	var version int
	if err := db.SQL.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}
