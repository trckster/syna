package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"syna/internal/common/protocol"
)

type DB struct {
	SQL *sql.DB
}

type WorkspaceState struct {
	ServerURL        string
	WorkspaceID      string
	SessionToken     string
	SessionExpiresAt time.Time
	LastServerSeq    int64
	ConnectionState  protocol.ConnectionState
	LastErrorKind    protocol.DaemonIssueKind
	LastError        string
}

type Root struct {
	RootID            string
	Kind              protocol.RootKind
	HomeRelPath       string
	TargetAbsPath     string
	State             protocol.RootState
	LatestSnapshotSeq int64
	LastScanAt        sql.NullTime
}

type Entry struct {
	RootID        string
	RelPath       string
	PathID        string
	Kind          protocol.RootKind
	CurrentSeq    int64
	ContentSHA256 string
	SizeBytes     int64
	Mode          int64
	MTimeNS       int64
	Inode         int64
	Device        int64
	Deleted       bool
}

type PendingOp struct {
	OpID        string
	RootID      string
	RelPath     string
	OpType      string
	BaseSeq     int64
	PayloadJSON string
	Status      string
	RetryCount  int
	LastError   string
	NextRetryAt time.Time
	CreatedAt   time.Time
}

type IgnoreRule struct {
	RootID        string
	RelPath       string
	ExpiresAt     time.Time
	Kind          protocol.RootKind
	ContentSHA256 string
	Mode          int64
	MTimeNS       int64
	Deleted       bool
}

type Warning struct {
	Key       string
	Message   string
	CreatedAt time.Time
}

func Open(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	return &DB{SQL: sqlDB}, nil
}

func (db *DB) Close() error {
	return db.SQL.Close()
}

func (db *DB) Migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS workspace_state (
			singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
			server_url TEXT NULL,
			workspace_id TEXT NULL,
			session_token TEXT NULL,
			session_expires_at TIMESTAMP NULL,
			last_server_seq INTEGER NOT NULL,
			connection_state TEXT NOT NULL,
			last_error_kind TEXT NULL,
			last_error TEXT NULL
		);`,
		`INSERT OR IGNORE INTO workspace_state
			(singleton, last_server_seq, connection_state)
			VALUES (1, 0, 'disconnected');`,
		`CREATE TABLE IF NOT EXISTS roots (
			root_id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			home_rel_path TEXT NOT NULL,
			target_abs_path TEXT NOT NULL,
			state TEXT NOT NULL,
			latest_snapshot_seq INTEGER NOT NULL DEFAULT 0,
			last_scan_at TIMESTAMP NULL
		);`,
		`CREATE TABLE IF NOT EXISTS entries (
			root_id TEXT NOT NULL,
			rel_path TEXT NOT NULL,
			path_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			current_seq INTEGER NOT NULL,
			content_sha256 TEXT NULL,
			size_bytes INTEGER NULL,
			mode INTEGER NOT NULL,
			mtime_ns INTEGER NOT NULL,
			inode INTEGER NULL,
			device INTEGER NULL,
			deleted INTEGER NOT NULL,
			PRIMARY KEY (root_id, rel_path)
		);`,
		`CREATE TABLE IF NOT EXISTS pending_ops (
			op_id TEXT PRIMARY KEY,
			root_id TEXT NOT NULL,
			rel_path TEXT NOT NULL,
			op_type TEXT NOT NULL,
			base_seq INTEGER NOT NULL,
			payload_json TEXT NOT NULL,
			status TEXT NOT NULL,
			retry_count INTEGER NOT NULL,
			last_error TEXT NULL,
			next_retry_at TIMESTAMP NULL,
			created_at TIMESTAMP NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS ignore_events (
			root_id TEXT NOT NULL,
			rel_path TEXT NOT NULL,
			expires_at TIMESTAMP NOT NULL,
			kind TEXT NULL,
			content_sha256 TEXT NULL,
			mode INTEGER NULL,
			mtime_ns INTEGER NULL,
			deleted INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (root_id, rel_path)
		);`,
		`CREATE TABLE IF NOT EXISTS warnings (
			key TEXT PRIMARY KEY,
			message TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL
		);`,
	}
	for _, stmt := range stmts {
		if _, err := db.SQL.Exec(stmt); err != nil {
			return err
		}
	}
	for _, stmt := range []string{
		`ALTER TABLE ignore_events ADD COLUMN kind TEXT NULL`,
		`ALTER TABLE ignore_events ADD COLUMN content_sha256 TEXT NULL`,
		`ALTER TABLE ignore_events ADD COLUMN mode INTEGER NULL`,
		`ALTER TABLE ignore_events ADD COLUMN mtime_ns INTEGER NULL`,
		`ALTER TABLE ignore_events ADD COLUMN deleted INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE workspace_state ADD COLUMN last_error_kind TEXT NULL`,
		`ALTER TABLE pending_ops ADD COLUMN next_retry_at TIMESTAMP NULL`,
	} {
		if _, err := db.SQL.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
}

func (db *DB) LoadWorkspaceState() (WorkspaceState, error) {
	var st WorkspaceState
	var serverURL sql.NullString
	var workspaceID sql.NullString
	var sessionToken sql.NullString
	var sessionExpiresAt sql.NullTime
	var lastErrorKind sql.NullString
	var lastError sql.NullString
	err := db.SQL.QueryRow(`
		SELECT server_url, workspace_id, session_token, session_expires_at, last_server_seq, connection_state, last_error_kind, last_error
		FROM workspace_state WHERE singleton = 1
	`).Scan(&serverURL, &workspaceID, &sessionToken, &sessionExpiresAt, &st.LastServerSeq, &st.ConnectionState, &lastErrorKind, &lastError)
	if err != nil {
		return st, err
	}
	if serverURL.Valid {
		st.ServerURL = serverURL.String
	}
	if workspaceID.Valid {
		st.WorkspaceID = workspaceID.String
	}
	if sessionToken.Valid {
		st.SessionToken = sessionToken.String
	}
	if sessionExpiresAt.Valid {
		st.SessionExpiresAt = sessionExpiresAt.Time
	}
	if lastErrorKind.Valid {
		st.LastErrorKind = protocol.DaemonIssueKind(lastErrorKind.String)
	}
	if lastError.Valid {
		st.LastError = lastError.String
	}
	return st, err
}

func (db *DB) SaveWorkspaceState(st WorkspaceState) error {
	_, err := db.SQL.Exec(`
		UPDATE workspace_state
		SET server_url = ?, workspace_id = ?, session_token = ?, session_expires_at = ?, last_server_seq = ?, connection_state = ?, last_error_kind = ?, last_error = ?
		WHERE singleton = 1
	`, nullString(st.ServerURL), nullString(st.WorkspaceID), nullString(st.SessionToken), nullTime(st.SessionExpiresAt), st.LastServerSeq, string(st.ConnectionState), nullString(string(st.LastErrorKind)), nullString(st.LastError))
	return err
}

func (db *DB) SetConnectionState(state protocol.ConnectionState, lastError string) error {
	return db.SetConnectionStateWithKind(state, "", lastError)
}

func (db *DB) SetConnectionStateWithKind(state protocol.ConnectionState, lastErrorKind protocol.DaemonIssueKind, lastError string) error {
	_, err := db.SQL.Exec(`UPDATE workspace_state SET connection_state = ?, last_error_kind = ?, last_error = ? WHERE singleton = 1`, string(state), nullString(string(lastErrorKind)), nullString(lastError))
	return err
}

func (db *DB) AdvanceLastSeq(seq int64) error {
	_, err := db.SQL.Exec(`UPDATE workspace_state SET last_server_seq = ? WHERE singleton = 1`, seq)
	return err
}

func (db *DB) ClearWorkspace() error {
	tx, err := db.SQL.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DELETE FROM roots`,
		`DELETE FROM entries`,
		`DELETE FROM pending_ops`,
		`DELETE FROM ignore_events`,
		`DELETE FROM warnings`,
		`UPDATE workspace_state SET server_url = NULL, workspace_id = NULL, session_token = NULL, session_expires_at = NULL, last_server_seq = 0, connection_state = 'disconnected', last_error_kind = NULL, last_error = NULL WHERE singleton = 1`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) UpsertRoot(root Root) error {
	_, err := db.SQL.Exec(`
		INSERT INTO roots (root_id, kind, home_rel_path, target_abs_path, state, latest_snapshot_seq, last_scan_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(root_id) DO UPDATE SET
			kind = excluded.kind,
			home_rel_path = excluded.home_rel_path,
			target_abs_path = excluded.target_abs_path,
			state = excluded.state,
			latest_snapshot_seq = excluded.latest_snapshot_seq,
			last_scan_at = excluded.last_scan_at
	`, root.RootID, string(root.Kind), root.HomeRelPath, root.TargetAbsPath, string(root.State), root.LatestSnapshotSeq, nullNullTime(root.LastScanAt))
	return err
}

func (db *DB) ListRoots() ([]Root, error) {
	rows, err := db.SQL.Query(`SELECT root_id, kind, home_rel_path, target_abs_path, state, latest_snapshot_seq, last_scan_at FROM roots ORDER BY home_rel_path ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roots []Root
	for rows.Next() {
		var root Root
		if err := rows.Scan(&root.RootID, &root.Kind, &root.HomeRelPath, &root.TargetAbsPath, &root.State, &root.LatestSnapshotSeq, &root.LastScanAt); err != nil {
			return nil, err
		}
		roots = append(roots, root)
	}
	return roots, rows.Err()
}

func (db *DB) RootByHomeRel(homeRelPath string) (*Root, error) {
	var root Root
	err := db.SQL.QueryRow(`
		SELECT root_id, kind, home_rel_path, target_abs_path, state, latest_snapshot_seq, last_scan_at
		FROM roots WHERE home_rel_path = ?
	`, homeRelPath).Scan(&root.RootID, &root.Kind, &root.HomeRelPath, &root.TargetAbsPath, &root.State, &root.LatestSnapshotSeq, &root.LastScanAt)
	if err != nil {
		return nil, err
	}
	return &root, nil
}

func (db *DB) RootByID(rootID string) (*Root, error) {
	var root Root
	err := db.SQL.QueryRow(`
		SELECT root_id, kind, home_rel_path, target_abs_path, state, latest_snapshot_seq, last_scan_at
		FROM roots WHERE root_id = ?
	`, rootID).Scan(&root.RootID, &root.Kind, &root.HomeRelPath, &root.TargetAbsPath, &root.State, &root.LatestSnapshotSeq, &root.LastScanAt)
	if err != nil {
		return nil, err
	}
	return &root, nil
}

func (db *DB) SetRootState(rootID string, state protocol.RootState) error {
	_, err := db.SQL.Exec(`UPDATE roots SET state = ? WHERE root_id = ?`, string(state), rootID)
	return err
}

func (db *DB) DeleteRootState(rootID string) error {
	tx, err := db.SQL.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []struct {
		q string
	}{
		{`DELETE FROM entries WHERE root_id = ?`},
		{`DELETE FROM pending_ops WHERE root_id = ?`},
		{`DELETE FROM ignore_events WHERE root_id = ?`},
	} {
		if _, err := tx.Exec(stmt.q, rootID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) ReplaceEntries(rootID string, entries []Entry) error {
	tx, err := db.SQL.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM entries WHERE root_id = ?`, rootID); err != nil {
		return err
	}
	for _, entry := range entries {
		if err := insertEntry(tx, entry); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) UpsertEntry(entry Entry) error {
	return insertEntry(db.SQL, entry)
}

func (db *DB) MarkEntryDeleted(entry Entry) error {
	entry.Deleted = true
	entry.ContentSHA256 = ""
	entry.SizeBytes = 0
	entry.Mode = 0
	entry.MTimeNS = 0
	entry.Inode = 0
	entry.Device = 0
	return insertEntry(db.SQL, entry)
}

func insertEntry(exec interface {
	Exec(query string, args ...any) (sql.Result, error)
}, entry Entry) error {
	_, err := exec.Exec(`
		INSERT INTO entries (root_id, rel_path, path_id, kind, current_seq, content_sha256, size_bytes, mode, mtime_ns, inode, device, deleted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(root_id, rel_path) DO UPDATE SET
			path_id = excluded.path_id,
			kind = excluded.kind,
			current_seq = excluded.current_seq,
			content_sha256 = excluded.content_sha256,
			size_bytes = excluded.size_bytes,
			mode = excluded.mode,
			mtime_ns = excluded.mtime_ns,
			inode = excluded.inode,
			device = excluded.device,
			deleted = excluded.deleted
	`, entry.RootID, entry.RelPath, entry.PathID, string(entry.Kind), entry.CurrentSeq, nullString(entry.ContentSHA256), nullInt64(entry.SizeBytes), entry.Mode, entry.MTimeNS, nullInt64(entry.Inode), nullInt64(entry.Device), boolToInt(entry.Deleted))
	return err
}

func (db *DB) DeleteEntry(rootID, relPath string) error {
	_, err := db.SQL.Exec(`DELETE FROM entries WHERE root_id = ? AND rel_path = ?`, rootID, relPath)
	return err
}

func (db *DB) EntriesForRoot(rootID string) (map[string]Entry, error) {
	rows, err := db.SQL.Query(`
		SELECT root_id, rel_path, path_id, kind, current_seq, content_sha256, size_bytes, mode, mtime_ns, inode, device, deleted
		FROM entries WHERE root_id = ?
	`, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]Entry)
	for rows.Next() {
		var entry Entry
		var contentSHA sql.NullString
		var sizeBytes sql.NullInt64
		var inode sql.NullInt64
		var device sql.NullInt64
		var deleted int
		if err := rows.Scan(&entry.RootID, &entry.RelPath, &entry.PathID, &entry.Kind, &entry.CurrentSeq, &contentSHA, &sizeBytes, &entry.Mode, &entry.MTimeNS, &inode, &device, &deleted); err != nil {
			return nil, err
		}
		if contentSHA.Valid {
			entry.ContentSHA256 = contentSHA.String
		}
		if sizeBytes.Valid {
			entry.SizeBytes = sizeBytes.Int64
		}
		if inode.Valid {
			entry.Inode = inode.Int64
		}
		if device.Valid {
			entry.Device = device.Int64
		}
		entry.Deleted = deleted == 1
		out[entry.RelPath] = entry
	}
	return out, rows.Err()
}

func (db *DB) CountPendingOps() (int64, error) {
	var n int64
	err := db.SQL.QueryRow(`SELECT COUNT(*) FROM pending_ops`).Scan(&n)
	return n, err
}

func (db *DB) ListPendingOps() ([]PendingOp, error) {
	rows, err := db.SQL.Query(`
		SELECT op_id, root_id, rel_path, op_type, base_seq, payload_json, status, retry_count, last_error, next_retry_at, created_at
		FROM pending_ops
		ORDER BY created_at ASC, op_id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPendingOps(rows)
}

func (db *DB) ListPendingOpsReady(now time.Time) ([]PendingOp, error) {
	rows, err := db.SQL.Query(`
		SELECT op_id, root_id, rel_path, op_type, base_seq, payload_json, status, retry_count, last_error, next_retry_at, created_at
		FROM pending_ops
		WHERE next_retry_at IS NULL OR next_retry_at <= ?
		ORDER BY created_at ASC, op_id ASC
	`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPendingOps(rows)
}

func scanPendingOps(rows *sql.Rows) ([]PendingOp, error) {
	var ops []PendingOp
	for rows.Next() {
		var op PendingOp
		var lastError sql.NullString
		var nextRetryAt sql.NullTime
		if err := rows.Scan(&op.OpID, &op.RootID, &op.RelPath, &op.OpType, &op.BaseSeq, &op.PayloadJSON, &op.Status, &op.RetryCount, &lastError, &nextRetryAt, &op.CreatedAt); err != nil {
			return nil, err
		}
		if lastError.Valid {
			op.LastError = lastError.String
		}
		if nextRetryAt.Valid {
			op.NextRetryAt = nextRetryAt.Time
		}
		ops = append(ops, op)
	}
	return ops, rows.Err()
}

func (db *DB) QueueRootRescan(rootID string, createdAt time.Time) error {
	var existing string
	err := db.SQL.QueryRow(`
		SELECT op_id
		FROM pending_ops
		WHERE root_id = ? AND rel_path = '' AND op_type = 'rescan_root'
		ORDER BY created_at ASC
		LIMIT 1
	`, rootID).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = db.SQL.Exec(`
			INSERT INTO pending_ops (op_id, root_id, rel_path, op_type, base_seq, payload_json, status, retry_count, last_error, next_retry_at, created_at)
			VALUES (?, ?, '', 'rescan_root', 0, '{}', 'queued', 0, NULL, NULL, ?)
		`, pendingOpID(rootID, createdAt), rootID, createdAt)
		return err
	case err != nil:
		return err
	default:
		_, err = db.SQL.Exec(`UPDATE pending_ops SET status = 'queued', next_retry_at = NULL WHERE op_id = ?`, existing)
		return err
	}
}

func (db *DB) DeletePendingOp(opID string) error {
	_, err := db.SQL.Exec(`DELETE FROM pending_ops WHERE op_id = ?`, opID)
	return err
}

func (db *DB) BumpPendingOpRetry(opID string, lastError string, now time.Time) error {
	var retryCount int
	if err := db.SQL.QueryRow(`SELECT retry_count FROM pending_ops WHERE op_id = ?`, opID).Scan(&retryCount); err != nil {
		return err
	}
	nextRetryAt := now.UTC().Add(pendingOpBackoff(retryCount))
	_, err := db.SQL.Exec(`
		UPDATE pending_ops
		SET status = 'queued', retry_count = retry_count + 1, last_error = ?, next_retry_at = ?
		WHERE op_id = ?
	`, nullString(lastError), nextRetryAt, opID)
	return err
}

func (db *DB) SetIgnore(rootID, relPath string, expiresAt time.Time, expected Entry) error {
	_, err := db.SQL.Exec(`
		INSERT INTO ignore_events (root_id, rel_path, expires_at, kind, content_sha256, mode, mtime_ns, deleted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(root_id, rel_path) DO UPDATE SET
			expires_at = excluded.expires_at,
			kind = excluded.kind,
			content_sha256 = excluded.content_sha256,
			mode = excluded.mode,
			mtime_ns = excluded.mtime_ns,
			deleted = excluded.deleted
	`, rootID, relPath, expiresAt, nullString(string(expected.Kind)), nullString(expected.ContentSHA256), expected.Mode, expected.MTimeNS, boolToInt(expected.Deleted))
	return err
}

func (db *DB) SetIgnoreDelete(rootID, relPath string, expiresAt time.Time) error {
	return db.SetIgnore(rootID, relPath, expiresAt, Entry{Deleted: true})
}

func (db *DB) IgnoreRule(rootID, relPath string, now time.Time) (IgnoreRule, bool, error) {
	var rule IgnoreRule
	var kind sql.NullString
	var contentSHA sql.NullString
	var mode sql.NullInt64
	var mtimeNS sql.NullInt64
	var deleted int
	err := db.SQL.QueryRow(`
		SELECT expires_at, kind, content_sha256, mode, mtime_ns, deleted
		FROM ignore_events
		WHERE root_id = ? AND rel_path = ?
	`, rootID, relPath).Scan(&rule.ExpiresAt, &kind, &contentSHA, &mode, &mtimeNS, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return IgnoreRule{}, false, nil
	}
	if err != nil {
		return IgnoreRule{}, false, err
	}
	rule.RootID = rootID
	rule.RelPath = relPath
	if kind.Valid {
		rule.Kind = protocol.RootKind(kind.String)
	}
	if contentSHA.Valid {
		rule.ContentSHA256 = contentSHA.String
	}
	if mode.Valid {
		rule.Mode = mode.Int64
	}
	if mtimeNS.Valid {
		rule.MTimeNS = mtimeNS.Int64
	}
	rule.Deleted = deleted == 1
	return rule, rule.ExpiresAt.After(now), nil
}

func (db *DB) IsIgnored(rootID, relPath string, now time.Time) (bool, error) {
	_, ignored, err := db.IgnoreRule(rootID, relPath, now)
	return ignored, err
}

func (db *DB) UpsertWarning(key, message string, createdAt time.Time) error {
	_, err := db.SQL.Exec(`
		INSERT INTO warnings (key, message, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			message = excluded.message,
			created_at = excluded.created_at
	`, key, message, createdAt)
	return err
}

func (db *DB) ClearWarningsWithPrefix(prefix string) error {
	_, err := db.SQL.Exec(`DELETE FROM warnings WHERE key LIKE ?`, prefix+"%")
	return err
}

func (db *DB) ListWarnings() ([]Warning, error) {
	rows, err := db.SQL.Query(`SELECT key, message, created_at FROM warnings ORDER BY created_at ASC, key ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var warnings []Warning
	for rows.Next() {
		var warning Warning
		if err := rows.Scan(&warning.Key, &warning.Message, &warning.CreatedAt); err != nil {
			return nil, err
		}
		warnings = append(warnings, warning)
	}
	return warnings, rows.Err()
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullTime(v time.Time) any {
	if v.IsZero() {
		return nil
	}
	return v
}

func nullNullTime(v sql.NullTime) any {
	if !v.Valid {
		return nil
	}
	return v.Time
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func pendingOpID(rootID string, createdAt time.Time) string {
	return rootID + ":" + createdAt.UTC().Format(time.RFC3339Nano)
}

func pendingOpBackoff(retryCount int) time.Duration {
	if retryCount < 0 {
		retryCount = 0
	}
	if retryCount > 6 {
		retryCount = 6
	}
	return time.Duration(1<<retryCount) * time.Second
}
