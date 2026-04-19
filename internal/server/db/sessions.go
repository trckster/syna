package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

func (db *DB) SaveChallenge(workspaceID, deviceID, deviceName string, clientNonce, serverNonce []byte) error {
	now := time.Now().UTC()
	_, err := db.SQL.Exec(`
		INSERT OR REPLACE INTO session_challenges
		(workspace_id, device_id, client_nonce, server_nonce, device_name, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, workspaceID, deviceID, clientNonce, serverNonce, deviceName, now.Add(60*time.Second), now)
	return err
}

func (db *DB) LoadChallenge(workspaceID, deviceID string, clientNonce []byte) ([]byte, string, time.Time, error) {
	var serverNonce []byte
	var deviceName string
	var expiresAt time.Time
	err := db.SQL.QueryRow(`
		SELECT server_nonce, device_name, expires_at
		FROM session_challenges
		WHERE workspace_id = ? AND device_id = ? AND client_nonce = ?
	`, workspaceID, deviceID, clientNonce).Scan(&serverNonce, &deviceName, &expiresAt)
	return serverNonce, deviceName, expiresAt, err
}

func (db *DB) DeleteChallenge(workspaceID, deviceID string, clientNonce []byte) error {
	_, err := db.SQL.Exec(`
		DELETE FROM session_challenges
		WHERE workspace_id = ? AND device_id = ? AND client_nonce = ?
	`, workspaceID, deviceID, clientNonce)
	return err
}

func (db *DB) CreateSession(workspaceID, deviceID, deviceName string, ttl time.Duration) (string, time.Time, int64, error) {
	tokenBytes := make([]byte, 32)
	if _, err := ioRand(tokenBytes); err != nil {
		return "", time.Time{}, 0, err
	}
	token := hex.EncodeToString(tokenBytes)
	tokenHash := sha256.Sum256([]byte(token))
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)
	currentSeq, err := db.CurrentSeq(workspaceID)
	if err != nil {
		return "", time.Time{}, 0, err
	}
	tx, err := db.Begin(context.Background())
	if err != nil {
		return "", time.Time{}, 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		INSERT OR REPLACE INTO sessions (token_hash, workspace_id, device_id, issued_at, expires_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, tokenHash[:], workspaceID, deviceID, now, expiresAt, now); err != nil {
		return "", time.Time{}, 0, err
	}
	if _, err := tx.Exec(`
		INSERT INTO devices (workspace_id, device_id, display_name, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(workspace_id, device_id) DO UPDATE SET
			display_name = excluded.display_name,
			last_seen_at = excluded.last_seen_at
	`, workspaceID, deviceID, deviceName, now, now); err != nil {
		return "", time.Time{}, 0, err
	}
	if err := tx.Commit(); err != nil {
		return "", time.Time{}, 0, err
	}
	return token, expiresAt, currentSeq, nil
}

func (db *DB) Authenticate(token string) (*Session, error) {
	sum := sha256.Sum256([]byte(token))
	var sess Session
	err := db.SQL.QueryRow(`
		SELECT workspace_id, device_id, expires_at
		FROM sessions WHERE token_hash = ?
	`, sum[:]).Scan(&sess.WorkspaceID, &sess.DeviceID, &sess.ExpiresAt)
	if err != nil {
		return nil, err
	}
	if time.Now().UTC().After(sess.ExpiresAt) {
		return nil, errors.New("session expired")
	}
	_, _ = db.SQL.Exec(`UPDATE sessions SET last_used_at = ? WHERE token_hash = ?`, time.Now().UTC(), sum[:])
	return &sess, nil
}

var ioRand = func(p []byte) (int, error) {
	return rand.Read(p)
}
