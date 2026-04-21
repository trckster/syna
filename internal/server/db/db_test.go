package db

import (
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"

	"syna/internal/common/protocol"
)

func TestRootRemoveRecomputesRetainedFloor(t *testing.T) {
	database := openTestDB(t)
	sess := testSession()

	if _, err := database.EnsureWorkspace(sess.WorkspaceID, []byte("public-key")); err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	if _, err := database.SubmitEvent(sess, protocol.EventSubmitRequest{
		RootID:      "root-1",
		RootKind:    protocol.RootKindDir,
		EventType:   protocol.EventRootAdd,
		PayloadBlob: "descriptor",
	}); err != nil {
		t.Fatalf("SubmitEvent(root_add): %v", err)
	}
	if _, err := database.SubmitEvent(sess, protocol.EventSubmitRequest{
		RootID:      "root-1",
		EventType:   protocol.EventRootRemove,
		PayloadBlob: "remove",
	}); err != nil {
		t.Fatalf("SubmitEvent(root_remove): %v", err)
	}

	floor, err := database.RetainedFloor(sess.WorkspaceID)
	if err != nil {
		t.Fatalf("RetainedFloor: %v", err)
	}
	currentSeq, err := database.CurrentSeq(sess.WorkspaceID)
	if err != nil {
		t.Fatalf("CurrentSeq: %v", err)
	}
	if floor != currentSeq {
		t.Fatalf("retained floor mismatch: got %d want %d", floor, currentSeq)
	}
}

func TestTransferredBytesMetricAccumulates(t *testing.T) {
	database := openTestDB(t)

	got, err := database.TransferredBytes()
	if err != nil {
		t.Fatalf("TransferredBytes(initial): %v", err)
	}
	if got != 0 {
		t.Fatalf("initial transferred bytes = %d want 0", got)
	}
	if err := database.AddTransferredBytes(100); err != nil {
		t.Fatalf("AddTransferredBytes(100): %v", err)
	}
	if err := database.AddTransferredBytes(23); err != nil {
		t.Fatalf("AddTransferredBytes(23): %v", err)
	}
	if err := database.AddTransferredBytes(0); err != nil {
		t.Fatalf("AddTransferredBytes(0): %v", err)
	}
	got, err = database.TransferredBytes()
	if err != nil {
		t.Fatalf("TransferredBytes(final): %v", err)
	}
	if got != 123 {
		t.Fatalf("transferred bytes = %d want 123", got)
	}
}

func TestSnapshotRefsAndPrune(t *testing.T) {
	database := openTestDB(t)
	sess := testSession()

	if _, err := database.EnsureWorkspace(sess.WorkspaceID, []byte("public-key")); err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	insertObject(t, database, "chunk-1")
	insertObject(t, database, "snapshot-1")

	if _, err := database.SubmitEvent(sess, protocol.EventSubmitRequest{
		RootID:      "root-1",
		RootKind:    protocol.RootKindDir,
		EventType:   protocol.EventRootAdd,
		PayloadBlob: "descriptor",
	}); err != nil {
		t.Fatalf("SubmitEvent(root_add): %v", err)
	}
	putResp, err := database.SubmitEvent(sess, protocol.EventSubmitRequest{
		RootID:      "root-1",
		PathID:      "path-1",
		EventType:   protocol.EventFilePut,
		BaseSeq:     ptrInt64(0),
		PayloadBlob: "file-put",
		ObjectRefs:  []string{"chunk-1"},
	})
	if err != nil {
		t.Fatalf("SubmitEvent(file_put): %v", err)
	}
	if err := database.SaveSnapshot(sess, protocol.SnapshotSubmitRequest{
		RootID:     "root-1",
		BaseSeq:    putResp.AcceptedSeq,
		ObjectID:   "snapshot-1",
		ObjectRefs: []string{"chunk-1"},
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	assertObjectRefCount(t, database, "chunk-1", 2)
	assertObjectRefCount(t, database, "snapshot-1", 1)

	now := time.Now().UTC().Add(time.Hour)
	deletedEvents, deletedSnapshots, objectIDs, err := database.Prune(now, 0, 24*time.Hour)
	if err != nil {
		t.Fatalf("Prune(events): %v", err)
	}
	if deletedEvents != 2 {
		t.Fatalf("deleted_events mismatch: got %d want 2", deletedEvents)
	}
	if deletedSnapshots != 0 {
		t.Fatalf("deleted_snapshots mismatch: got %d want 0", deletedSnapshots)
	}
	if len(objectIDs) != 0 {
		t.Fatalf("expected no GC candidates yet, got %v", objectIDs)
	}
	assertObjectRefCount(t, database, "chunk-1", 1)
	assertObjectRefCount(t, database, "snapshot-1", 1)

	if _, err := database.SubmitEvent(sess, protocol.EventSubmitRequest{
		RootID:      "root-1",
		EventType:   protocol.EventRootRemove,
		PayloadBlob: "remove",
	}); err != nil {
		t.Fatalf("SubmitEvent(root_remove): %v", err)
	}
	_, deletedSnapshots, objectIDs, err = database.Prune(now.Add(time.Hour), 0, 0)
	if err != nil {
		t.Fatalf("Prune(snapshots): %v", err)
	}
	if deletedSnapshots != 1 {
		t.Fatalf("deleted_snapshots mismatch after remove: got %d want 1", deletedSnapshots)
	}
	if len(objectIDs) != 2 {
		t.Fatalf("expected two GC candidates, got %v", objectIDs)
	}
	assertObjectMissing(t, database, "chunk-1")
	assertObjectMissing(t, database, "snapshot-1")
}

func TestSnapshotRequiresActiveRootAndExistingRefs(t *testing.T) {
	database := openTestDB(t)
	sess := testSession()

	if _, err := database.EnsureWorkspace(sess.WorkspaceID, []byte("public-key")); err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	insertObject(t, database, "snapshot-1")

	if err := database.SaveSnapshot(sess, protocol.SnapshotSubmitRequest{
		RootID:   "missing-root",
		BaseSeq:  1,
		ObjectID: "snapshot-1",
	}); err == nil {
		t.Fatalf("expected snapshot for unknown root to be rejected")
	}

	if _, err := database.SubmitEvent(sess, protocol.EventSubmitRequest{
		RootID:      "root-1",
		RootKind:    protocol.RootKindDir,
		EventType:   protocol.EventRootAdd,
		PayloadBlob: "descriptor",
	}); err != nil {
		t.Fatalf("SubmitEvent(root_add): %v", err)
	}
	if err := database.SaveSnapshot(sess, protocol.SnapshotSubmitRequest{
		RootID:     "root-1",
		BaseSeq:    1,
		ObjectID:   "snapshot-1",
		ObjectRefs: []string{"missing-chunk"},
	}); err == nil {
		t.Fatalf("expected snapshot with missing object ref to be rejected")
	}
}

func TestSubmitEventRejectsUnsupportedEventType(t *testing.T) {
	database := openTestDB(t)
	sess := testSession()

	if _, err := database.EnsureWorkspace(sess.WorkspaceID, []byte("public-key")); err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	if _, err := database.SubmitEvent(sess, protocol.EventSubmitRequest{
		RootID:      "root-1",
		EventType:   protocol.EventType("unsupported"),
		PayloadBlob: "payload",
	}); err == nil {
		t.Fatalf("expected unsupported event type to be rejected")
	}
}

func TestSessionUsesChallengeDeviceName(t *testing.T) {
	database := openTestDB(t)
	sess := testSession()

	if _, err := database.EnsureWorkspace(sess.WorkspaceID, []byte("public-key")); err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}

	clientNonce := []byte("client-nonce")
	serverNonce := []byte("server-nonce")
	if err := database.SaveChallenge(sess.WorkspaceID, sess.DeviceID, "laptop", clientNonce, serverNonce); err != nil {
		t.Fatalf("SaveChallenge: %v", err)
	}

	loadedNonce, loadedName, _, err := database.LoadChallenge(sess.WorkspaceID, sess.DeviceID, clientNonce)
	if err != nil {
		t.Fatalf("LoadChallenge: %v", err)
	}
	if string(loadedNonce) != string(serverNonce) {
		t.Fatalf("server nonce mismatch: got %q want %q", string(loadedNonce), string(serverNonce))
	}
	if loadedName != "laptop" {
		t.Fatalf("device name mismatch: got %q want %q", loadedName, "laptop")
	}

	token, _, _, err := database.CreateSession(sess.WorkspaceID, sess.DeviceID, loadedName, time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	var displayName string
	if err := database.SQL.QueryRow(`
		SELECT display_name
		FROM devices
		WHERE workspace_id = ? AND device_id = ?
	`, sess.WorkspaceID, sess.DeviceID).Scan(&displayName); err != nil {
		t.Fatalf("query devices: %v", err)
	}
	if displayName != "laptop" {
		t.Fatalf("display name mismatch: got %q want %q", displayName, "laptop")
	}

	tokenHash := sha256.Sum256([]byte(token))
	var storedDeviceID string
	if err := database.SQL.QueryRow(`
		SELECT device_id
		FROM sessions
		WHERE token_hash = ?
	`, tokenHash[:]).Scan(&storedDeviceID); err != nil {
		t.Fatalf("query sessions: %v", err)
	}
	if storedDeviceID != sess.DeviceID {
		t.Fatalf("stored device ID mismatch: got %q want %q", storedDeviceID, sess.DeviceID)
	}
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	database, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return database
}

func testSession() *Session {
	return &Session{WorkspaceID: "workspace-1", DeviceID: "device-1"}
}

func insertObject(t *testing.T, database *DB, objectID string) {
	t.Helper()
	now := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := database.SQL.Exec(`
		INSERT INTO objects (object_id, kind, size_bytes, storage_rel_path, ref_count, zero_ref_at, created_at, last_accessed_at)
		VALUES (?, 'file_chunk', 1, ?, 0, NULL, ?, ?)
	`, objectID, "objects/"+objectID, now, now); err != nil {
		t.Fatalf("insertObject(%s): %v", objectID, err)
	}
}

func assertObjectRefCount(t *testing.T, database *DB, objectID string, want int) {
	t.Helper()
	var got int
	if err := database.SQL.QueryRow(`SELECT ref_count FROM objects WHERE object_id = ?`, objectID).Scan(&got); err != nil {
		t.Fatalf("ref_count(%s): %v", objectID, err)
	}
	if got != want {
		t.Fatalf("ref_count(%s) = %d want %d", objectID, got, want)
	}
}

func assertObjectMissing(t *testing.T, database *DB, objectID string) {
	t.Helper()
	var exists int
	err := database.SQL.QueryRow(`SELECT 1 FROM objects WHERE object_id = ?`, objectID).Scan(&exists)
	if err == nil {
		t.Fatalf("expected object %s to be deleted", objectID)
	}
}

func ptrInt64(v int64) *int64 {
	return &v
}
