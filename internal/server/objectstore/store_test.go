package objectstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"syna/internal/server/db"
)

func TestPutIsIdempotentUnderConcurrentUploads(t *testing.T) {
	dataDir := t.TempDir()
	for _, dir := range []string{
		filepath.Join(dataDir, "objects"),
		filepath.Join(dataDir, "tmp"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	database, err := db.Open(filepath.Join(dataDir, "state.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatalf("database.Migrate: %v", err)
	}
	store := New(dataDir)

	blob := []byte("object-bytes")
	sum := sha256.Sum256(blob)
	objectID := hex.EncodeToString(sum[:])

	var wg sync.WaitGroup
	results := make([]bool, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = store.Put(database.SQL, objectID, "snapshot", int64(len(blob)), int64(len(blob)), bytes.NewReader(blob))
		}(i)
	}
	wg.Wait()

	createdCount := 0
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("Put[%d]: %v", i, errs[i])
		}
		if results[i] {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("expected exactly one creator, got %d", createdCount)
	}
	var exists int
	if err := database.SQL.QueryRow(`SELECT 1 FROM objects WHERE object_id = ?`, objectID).Scan(&exists); err != nil {
		t.Fatalf("object metadata missing: %v", err)
	}
}
