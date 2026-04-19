package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"syna/internal/server/config"
	"syna/internal/server/db"
	"syna/internal/server/objectstore"
)

func TestGCDeletesZeroRefObjectFilesAfterRetention(t *testing.T) {
	dataDir := t.TempDir()
	if err := config.EnsureDataDirs(dataDir); err != nil {
		t.Fatalf("EnsureDataDirs: %v", err)
	}
	database, err := db.Open(filepath.Join(dataDir, "state.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	body := []byte("encrypted object")
	sum := sha256.Sum256(body)
	objectID := hex.EncodeToString(sum[:])
	store := objectstore.New(dataDir)
	objectPath := store.ObjectPath(objectID)
	if err := os.MkdirAll(filepath.Dir(objectPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(object): %v", err)
	}
	if err := os.WriteFile(objectPath, body, 0o600); err != nil {
		t.Fatalf("WriteFile(object): %v", err)
	}
	old := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := database.SQL.Exec(`
		INSERT INTO objects (object_id, kind, size_bytes, storage_rel_path, ref_count, zero_ref_at, created_at, last_accessed_at)
		VALUES (?, 'file_chunk', ?, ?, 0, ?, ?, ?)
	`, objectID, len(body), "objects/"+objectID, old, old, old); err != nil {
		t.Fatalf("insert object metadata: %v", err)
	}

	if err := GC(database, store, time.Now().UTC(), time.Hour, time.Hour); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if _, err := os.Stat(objectPath); !os.IsNotExist(err) {
		t.Fatalf("expected object file to be deleted, stat err=%v", err)
	}
}
