package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
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

func TestPurgeWorkspaceDeletesOnlyTargetWorkspaceData(t *testing.T) {
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

	targetID := strings.Repeat("a", 32)
	otherID := strings.Repeat("b", 32)
	for _, workspaceID := range []string{targetID, otherID} {
		if _, err := database.EnsureWorkspace(workspaceID, []byte("public-key-"+workspaceID)); err != nil {
			t.Fatalf("EnsureWorkspace(%s): %v", workspaceID, err)
		}
	}
	if _, _, _, err := database.CreateSession(targetID, "device-1", "laptop", time.Hour); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := database.SaveChallenge(targetID, "device-1", "laptop", []byte("client"), []byte("server")); err != nil {
		t.Fatalf("SaveChallenge: %v", err)
	}

	store := objectstore.New(dataDir)
	exclusiveID := writePurgeTestObject(t, database, store, []byte("exclusive encrypted bytes"))
	sharedID := writePurgeTestObject(t, database, store, []byte("shared encrypted bytes"))
	if err := database.AssociateObjectWithWorkspace(targetID, exclusiveID); err != nil {
		t.Fatalf("associate exclusive object: %v", err)
	}
	for _, workspaceID := range []string{targetID, otherID} {
		if err := database.AssociateObjectWithWorkspace(workspaceID, sharedID); err != nil {
			t.Fatalf("associate shared object: %v", err)
		}
	}
	now := time.Now().UTC()
	result, err := database.SQL.Exec(`
		INSERT INTO events (workspace_id, root_id, path_id, event_type, base_seq, author_device_id, payload_blob, created_at)
		VALUES (?, 'root-1', 'path-1', 'file_put', 0, 'device-1', X'01', ?)
	`, targetID, now)
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	eventSeq, _ := result.LastInsertId()
	if _, err := database.SQL.Exec(`INSERT INTO event_object_refs (event_seq, object_id) VALUES (?, ?)`, eventSeq, exclusiveID); err != nil {
		t.Fatalf("insert event ref: %v", err)
	}
	if _, err := database.SQL.Exec(`UPDATE objects SET ref_count = 1 WHERE object_id = ?`, exclusiveID); err != nil {
		t.Fatalf("update ref count: %v", err)
	}
	if _, err := database.SQL.Exec(`
		INSERT INTO roots (workspace_id, root_id, kind, descriptor_blob, created_seq, created_at, updated_at)
		VALUES (?, 'root-1', 'dir', X'01', ?, ?, ?)
	`, targetID, eventSeq, now, now); err != nil {
		t.Fatalf("insert root: %v", err)
	}

	var output bytes.Buffer
	if err := PurgeWorkspace(database, store, dataDir, targetID, &output); err != nil {
		t.Fatalf("PurgeWorkspace: %v", err)
	}
	if !strings.Contains(output.String(), "deleted_objects: 1") {
		t.Fatalf("unexpected output:\n%s", output.String())
	}
	for _, table := range []string{"workspaces", "devices", "sessions", "session_challenges", "roots", "events", "workspace_objects"} {
		var count int
		if err := database.SQL.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE workspace_id = ?`, targetID).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s retained %d target rows", table, count)
		}
	}
	if _, err := os.Stat(store.ObjectPath(exclusiveID)); !os.IsNotExist(err) {
		t.Fatalf("exclusive object was not deleted: %v", err)
	}
	if _, err := os.Stat(store.ObjectPath(sharedID)); err != nil {
		t.Fatalf("shared object was deleted: %v", err)
	}
	if _, err := database.WorkspacePubKey(otherID); err != nil {
		t.Fatalf("other workspace was deleted: %v", err)
	}
}

func TestRecoverPurgeStagingRestoresReferencedAndDeletesOrphanedObjects(t *testing.T) {
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
	store := objectstore.New(dataDir)
	referencedID := writePurgeTestObject(t, database, store, []byte("referenced"))
	orphanBytes := []byte("orphan")
	orphanSum := sha256.Sum256(orphanBytes)
	orphanID := hex.EncodeToString(orphanSum[:])
	stageDir, err := os.MkdirTemp(filepath.Join(dataDir, "tmp"), "purge-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	if err := os.Rename(store.ObjectPath(referencedID), filepath.Join(stageDir, referencedID+".bin")); err != nil {
		t.Fatalf("stage referenced object: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, orphanID+".bin"), orphanBytes, 0o600); err != nil {
		t.Fatalf("stage orphan object: %v", err)
	}
	if err := RecoverPurgeStaging(database, store, dataDir); err != nil {
		t.Fatalf("RecoverPurgeStaging: %v", err)
	}
	if _, err := os.Stat(store.ObjectPath(referencedID)); err != nil {
		t.Fatalf("referenced object was not restored: %v", err)
	}
	if _, err := os.Stat(stageDir); !os.IsNotExist(err) {
		t.Fatalf("staging directory remains: %v", err)
	}
}

func writePurgeTestObject(t *testing.T, database *db.DB, store *objectstore.Store, body []byte) string {
	t.Helper()
	sum := sha256.Sum256(body)
	objectID := hex.EncodeToString(sum[:])
	path := store.ObjectPath(objectID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	now := time.Now().UTC()
	if _, err := database.SQL.Exec(`
		INSERT INTO objects (object_id, kind, size_bytes, storage_rel_path, ref_count, zero_ref_at, created_at, last_accessed_at)
		VALUES (?, 'file_chunk', ?, ?, 0, ?, ?, ?)
	`, objectID, len(body), "objects/"+objectID, now, now, now); err != nil {
		t.Fatalf("insert object: %v", err)
	}
	return objectID
}
