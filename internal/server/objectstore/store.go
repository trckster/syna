package objectstore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const objectIDLength = sha256.Size * 2

type Store struct {
	dataDir string
}

func New(dataDir string) *Store {
	return &Store{dataDir: dataDir}
}

func (s *Store) ObjectPath(objectID string) string {
	return filepath.Join(s.dataDir, "objects", objectID[:2], objectID[2:4], objectID+".bin")
}

func (s *Store) Put(db *sql.DB, objectID, kind string, plainSize int64, maxBytes int64, body io.Reader) (bool, error) {
	if !ValidObjectID(objectID) {
		return false, fmt.Errorf("invalid object id")
	}
	if plainSize <= 0 {
		return false, fmt.Errorf("invalid plain size")
	}
	if maxBytes <= 0 {
		return false, fmt.Errorf("invalid upload size limit")
	}
	tmpFile, err := os.CreateTemp(filepath.Join(s.dataDir, "tmp"), "upload-*.bin")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	hash := sha256.New()
	limited := &io.LimitedReader{R: body, N: maxBytes + 1}
	size, err := io.Copy(io.MultiWriter(tmpFile, hash), limited)
	if err != nil {
		return false, err
	}
	if size > maxBytes {
		return false, fmt.Errorf("object upload exceeds %d bytes", maxBytes)
	}
	if size == 0 {
		return false, fmt.Errorf("empty object upload")
	}
	actualID := hex.EncodeToString(hash.Sum(nil))
	if actualID != objectID {
		return false, fmt.Errorf("object hash mismatch")
	}
	if err := tmpFile.Sync(); err != nil {
		return false, err
	}

	finalPath := s.ObjectPath(objectID)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return false, err
	}
	created := false
	if err := os.Link(tmpFile.Name(), finalPath); err != nil {
		if !os.IsExist(err) {
			return false, err
		}
	} else {
		created = true
	}

	now := time.Now().UTC()
	_, err = db.Exec(`
		INSERT INTO objects (object_id, kind, size_bytes, storage_rel_path, ref_count, zero_ref_at, created_at, last_accessed_at)
		VALUES (?, ?, ?, ?, 0, ?, ?, ?)
		ON CONFLICT(object_id) DO UPDATE SET
			last_accessed_at = excluded.last_accessed_at
	`, objectID, kind, size, relStorePath(objectID), now, now, now)
	if err != nil {
		return false, err
	}
	return created, nil
}

func (s *Store) Get(db *sql.DB, objectID string) (*os.File, error) {
	if !ValidObjectID(objectID) {
		return nil, fmt.Errorf("invalid object id")
	}
	if _, err := db.Exec(`UPDATE objects SET last_accessed_at = ? WHERE object_id = ?`, time.Now().UTC(), objectID); err != nil {
		return nil, err
	}
	return os.Open(s.ObjectPath(objectID))
}

func ValidObjectID(objectID string) bool {
	if len(objectID) != objectIDLength {
		return false
	}
	for _, r := range objectID {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

func relStorePath(objectID string) string {
	return filepath.ToSlash(filepath.Join("objects", objectID[:2], objectID[2:4], objectID+".bin"))
}
