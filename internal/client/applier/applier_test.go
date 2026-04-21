package applier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"syna/internal/client/state"
	commoncrypto "syna/internal/common/crypto"
	"syna/internal/common/protocol"
)

func TestApplyEventStageOnlyPreservesLocalDisk(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := state.Open(filepath.Join(tmpDir, "client.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	raw := bytes.Repeat([]byte{0x42}, 32)
	keys, err := commoncrypto.Derive(raw)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}

	root := state.Root{
		RootID:        "root-1",
		Kind:          protocol.RootKindDir,
		HomeRelPath:   "notes",
		TargetAbsPath: filepath.Join(tmpDir, "notes"),
		State:         protocol.RootStateActive,
	}
	if err := os.MkdirAll(root.TargetAbsPath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := db.UpsertRoot(root); err != nil {
		t.Fatalf("UpsertRoot: %v", err)
	}

	const workspaceID = "workspace-1"
	const relPath = "docs/a.txt"
	pathID := commoncrypto.PathID(keys, root.RootID, relPath)
	putPayload := protocol.FilePutPayload{
		Path:          relPath,
		Mode:          0o644,
		MTimeNS:       time.Now().UTC().UnixNano(),
		SizeBytes:     5,
		ContentSHA256: "abc123",
	}
	putEvent := mustEvent(t, keys, workspaceID, root.RootID, pathID, protocol.EventFilePut, putPayload)
	if err := ApplyEvent(context.Background(), nil, keys, workspaceID, root, putEvent, db, ApplyOptions{StageOnly: true}); err != nil {
		t.Fatalf("ApplyEvent(file_put): %v", err)
	}

	entries, err := db.EntriesForRoot(root.RootID)
	if err != nil {
		t.Fatalf("EntriesForRoot: %v", err)
	}
	entry, ok := entries[relPath]
	if !ok {
		t.Fatalf("expected staged entry for %q", relPath)
	}
	if entry.ContentSHA256 != putPayload.ContentSHA256 {
		t.Fatalf("content hash mismatch: got %q want %q", entry.ContentSHA256, putPayload.ContentSHA256)
	}
	if _, err := os.Stat(filepath.Join(root.TargetAbsPath, filepath.FromSlash(relPath))); !os.IsNotExist(err) {
		t.Fatalf("stage-only file_put should not materialize to disk, got err=%v", err)
	}

	localPath := filepath.Join(root.TargetAbsPath, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(local): %v", err)
	}
	if err := os.WriteFile(localPath, []byte("local bytes"), 0o644); err != nil {
		t.Fatalf("WriteFile(local): %v", err)
	}

	deleteEvent := mustEvent(t, keys, workspaceID, root.RootID, pathID, protocol.EventDelete, protocol.DeletePayload{Path: relPath})
	if err := ApplyEvent(context.Background(), nil, keys, workspaceID, root, deleteEvent, db, ApplyOptions{StageOnly: true}); err != nil {
		t.Fatalf("ApplyEvent(delete): %v", err)
	}
	if _, err := os.Stat(localPath); err != nil {
		t.Fatalf("stage-only delete should preserve local file: %v", err)
	}
	entries, err = db.EntriesForRoot(root.RootID)
	if err != nil {
		t.Fatalf("EntriesForRoot(after delete): %v", err)
	}
	entry, ok = entries[relPath]
	if !ok || !entry.Deleted {
		t.Fatalf("expected staged entry to be tombstoned after delete, got %+v ok=%v", entry, ok)
	}
	if entry.CurrentSeq != deleteEvent.Seq {
		t.Fatalf("delete current seq = %d want %d", entry.CurrentSeq, deleteEvent.Seq)
	}
}

func TestApplyEventRejectsTraversalPath(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := state.Open(filepath.Join(tmpDir, "client.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	keys, err := commoncrypto.Derive(bytes.Repeat([]byte{0x24}, 32))
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	root := state.Root{
		RootID:        "root-1",
		Kind:          protocol.RootKindDir,
		HomeRelPath:   "notes",
		TargetAbsPath: filepath.Join(tmpDir, "notes"),
		State:         protocol.RootStateActive,
	}
	if err := os.MkdirAll(root.TargetAbsPath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := db.UpsertRoot(root); err != nil {
		t.Fatalf("UpsertRoot: %v", err)
	}
	const workspaceID = "workspace-1"
	badPath := "../.ssh/authorized_keys"
	pathID := commoncrypto.PathID(keys, root.RootID, badPath)
	event := mustEvent(t, keys, workspaceID, root.RootID, pathID, protocol.EventFilePut, protocol.FilePutPayload{
		Path:          badPath,
		Mode:          0o600,
		MTimeNS:       time.Now().UTC().UnixNano(),
		SizeBytes:     0,
		ContentSHA256: "e3b0c44298fc1c149afbf4c8996fb924",
	})
	err = ApplyEvent(context.Background(), nil, keys, workspaceID, root, event, db, ApplyOptions{StageOnly: true})
	var integrityErr *IntegrityError
	if !errors.As(err, &integrityErr) {
		t.Fatalf("expected IntegrityError, got %v", err)
	}
}

func TestApplyEventRejectsMismatchedPathID(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := state.Open(filepath.Join(tmpDir, "client.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	keys, err := commoncrypto.Derive(bytes.Repeat([]byte{0x55}, 32))
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	root := state.Root{
		RootID:        "root-1",
		Kind:          protocol.RootKindDir,
		HomeRelPath:   "notes",
		TargetAbsPath: filepath.Join(tmpDir, "notes"),
		State:         protocol.RootStateActive,
	}
	if err := os.MkdirAll(root.TargetAbsPath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := db.UpsertRoot(root); err != nil {
		t.Fatalf("UpsertRoot: %v", err)
	}
	const workspaceID = "workspace-1"
	eventPathID := commoncrypto.PathID(keys, root.RootID, "other.txt")
	event := mustEvent(t, keys, workspaceID, root.RootID, eventPathID, protocol.EventDelete, protocol.DeletePayload{Path: "note.txt"})
	err = ApplyEvent(context.Background(), nil, keys, workspaceID, root, event, db, ApplyOptions{StageOnly: true})
	var integrityErr *IntegrityError
	if !errors.As(err, &integrityErr) {
		t.Fatalf("expected IntegrityError, got %v", err)
	}
}

func mustEvent(t *testing.T, keys *commoncrypto.DerivedKeys, workspaceID, rootID, pathID string, eventType protocol.EventType, payload any) protocol.EventRecord {
	t.Helper()
	plain, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	blob, err := commoncrypto.Encrypt(keys.EventKey, plain, commoncrypto.EventAAD(workspaceID, rootID, pathID, string(eventType)))
	if err != nil {
		t.Fatalf("Encrypt payload: %v", err)
	}
	return protocol.EventRecord{
		Seq:         7,
		RootID:      rootID,
		PathID:      &pathID,
		EventType:   eventType,
		PayloadBlob: commoncrypto.Base64Raw(blob),
		CreatedAt:   time.Now().UTC(),
	}
}
