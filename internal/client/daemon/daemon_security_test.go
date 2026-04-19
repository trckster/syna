package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"syna/internal/client/applier"
	"syna/internal/client/connector"
	"syna/internal/client/scanner"
	"syna/internal/client/state"
	"syna/internal/client/uploader"
	commoncrypto "syna/internal/common/crypto"
	"syna/internal/common/protocol"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestEnsureRootFromDescriptorRejectsTraversalHomeRel(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()

	keys, err := commoncrypto.Derive(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	d.keys = keys
	d.cfg.WorkspaceID = "workspace-1"

	payload := protocol.RootAddPayload{
		RootID:      commoncrypto.RootID(keys, "../../../etc"),
		Kind:        protocol.RootKindDir,
		HomeRelPath: "../../../etc",
	}
	root := mustBootstrapRoot(t, keys, d.cfg.WorkspaceID, payload.RootID, payload)
	_, err = d.ensureRootFromDescriptor(root)
	var integrityErr *applier.IntegrityError
	if !errors.As(err, &integrityErr) {
		t.Fatalf("expected integrity error, got %v", err)
	}
}

func TestEnsureRootFromDescriptorRejectsMismatchedRootID(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()

	keys, err := commoncrypto.Derive(bytes.Repeat([]byte{0x32}, 32))
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	d.keys = keys
	d.cfg.WorkspaceID = "workspace-1"

	payload := protocol.RootAddPayload{
		RootID:      "not-the-derived-root-id",
		Kind:        protocol.RootKindDir,
		HomeRelPath: "notes",
	}
	root := mustBootstrapRoot(t, keys, d.cfg.WorkspaceID, payload.RootID, payload)
	_, err = d.ensureRootFromDescriptor(root)
	if err == nil || !strings.Contains(err.Error(), "root_id") {
		t.Fatalf("expected root_id integrity error, got %v", err)
	}
}

func TestApplySnapshotRejectsTraversalEntry(t *testing.T) {
	d, root, cancel := newSecurityTestDaemon(t)
	defer cancel()

	snapshot := protocol.SnapshotPayload{
		RootID:      root.RootID,
		Kind:        root.Kind,
		HomeRelPath: root.HomeRelPath,
		BaseSeq:     7,
		Entries: []protocol.SnapshotEntry{
			{
				Path: "../escape.txt",
				Kind: protocol.RootKindFile,
				Mode: 0o644,
			},
		},
	}
	blob := mustSnapshotBlob(t, d.keys, d.cfg.WorkspaceID, root.RootID, 7, snapshot)
	d.conn = newStaticObjectClient("snapshot-1", blob)

	err := d.applySnapshot(context.Background(), root, "snapshot-1", 7, true)
	if err == nil || !strings.Contains(err.Error(), "snapshot entry") {
		t.Fatalf("expected snapshot path rejection, got %v", err)
	}
}

func TestBootstrapRejectsMaliciousRootAddDescriptor(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()

	keys, err := commoncrypto.Derive(bytes.Repeat([]byte{0x33}, 32))
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	d.keys = keys
	d.cfg.WorkspaceID = "workspace-1"
	payload := protocol.RootAddPayload{
		RootID:      "malicious-root",
		Kind:        protocol.RootKindDir,
		HomeRelPath: "../escape",
	}
	root := mustBootstrapRoot(t, keys, d.cfg.WorkspaceID, payload.RootID, payload)
	d.conn = newBootstrapObjectClient(t, protocol.BootstrapResponse{
		CurrentSeq:        9,
		BootstrapAfterSeq: 9,
		Roots:             []protocol.BootstrapRoot{root},
	}, protocol.EventFetchResponse{CurrentSeq: 9})

	if err := d.bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap should skip malicious descriptor: %v", err)
	}
	roots, err := d.stateDB.ListRoots()
	if err != nil {
		t.Fatalf("ListRoots: %v", err)
	}
	if len(roots) != 0 {
		t.Fatalf("malicious bootstrap descriptor created roots: %+v", roots)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(home), "escape")); !os.IsNotExist(err) {
		t.Fatalf("malicious bootstrap descriptor touched outside path, err=%v", err)
	}
}

func TestApplyRemoteRootAddRejectsTraversalHomeRel(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()

	keys, err := commoncrypto.Derive(bytes.Repeat([]byte{0x34}, 32))
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	d.keys = keys
	d.cfg.WorkspaceID = "workspace-1"
	payload := protocol.RootAddPayload{
		RootID:      "malicious-root",
		Kind:        protocol.RootKindDir,
		HomeRelPath: "../escape",
	}
	descriptor := mustBootstrapRoot(t, keys, d.cfg.WorkspaceID, payload.RootID, payload)
	err = d.applyRemoteEvent(context.Background(), protocol.EventRecord{
		Seq:         11,
		RootID:      payload.RootID,
		EventType:   protocol.EventRootAdd,
		PayloadBlob: descriptor.DescriptorBlob,
	})
	if err != nil {
		t.Fatalf("applyRemoteEvent(root_add) should consume malicious event: %v", err)
	}
	roots, err := d.stateDB.ListRoots()
	if err != nil {
		t.Fatalf("ListRoots: %v", err)
	}
	if len(roots) != 0 {
		t.Fatalf("malicious root_add created roots: %+v", roots)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(home), "escape")); !os.IsNotExist(err) {
		t.Fatalf("malicious root_add touched outside path, err=%v", err)
	}
}

func TestApplyRemoteDirPutRejectsTraversalPath(t *testing.T) {
	d, root, cancel := newSecurityTestDaemon(t)
	defer cancel()
	if err := os.MkdirAll(root.TargetAbsPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}

	badPath := "../escape-dir"
	pathID := commoncrypto.PathID(d.keys, root.RootID, badPath)
	event := mustDaemonEvent(t, d.keys, d.cfg.WorkspaceID, root.RootID, pathID, protocol.EventDirPut, protocol.DirPutPayload{
		Path:    badPath,
		Mode:    0o755,
		MTimeNS: time.Now().UTC().UnixNano(),
	})
	if err := d.applyRemoteEvent(context.Background(), event); err != nil {
		t.Fatalf("applyRemoteEvent(dir_put) should consume malicious event: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root.TargetAbsPath), "escape-dir")); !os.IsNotExist(err) {
		t.Fatalf("malicious dir_put touched outside path, err=%v", err)
	}
}

func TestApplyRemoteDeleteRejectsTraversalPath(t *testing.T) {
	d, root, cancel := newSecurityTestDaemon(t)
	defer cancel()
	if err := os.MkdirAll(root.TargetAbsPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	outside := filepath.Join(filepath.Dir(root.TargetAbsPath), "keep.txt")
	if err := os.WriteFile(outside, []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(outside): %v", err)
	}

	badPath := "../keep.txt"
	pathID := commoncrypto.PathID(d.keys, root.RootID, badPath)
	event := mustDaemonEvent(t, d.keys, d.cfg.WorkspaceID, root.RootID, pathID, protocol.EventDelete, protocol.DeletePayload{Path: badPath})
	if err := d.applyRemoteEvent(context.Background(), event); err != nil {
		t.Fatalf("applyRemoteEvent(delete) should consume malicious event: %v", err)
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("outside file should remain: %v", err)
	}
	if string(got) != "keep\n" {
		t.Fatalf("outside file changed to %q", string(got))
	}
}

func TestApplySnapshotRejectsTraversalEntryDuringMaterialization(t *testing.T) {
	d, root, cancel := newSecurityTestDaemon(t)
	defer cancel()
	if err := os.MkdirAll(root.TargetAbsPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}

	snapshot := protocol.SnapshotPayload{
		RootID:      root.RootID,
		Kind:        root.Kind,
		HomeRelPath: root.HomeRelPath,
		BaseSeq:     7,
		Entries: []protocol.SnapshotEntry{
			{
				Path: "../escape.txt",
				Kind: protocol.RootKindFile,
				Mode: 0o644,
			},
		},
	}
	blob := mustSnapshotBlob(t, d.keys, d.cfg.WorkspaceID, root.RootID, 7, snapshot)
	d.conn = newStaticObjectClient("snapshot-3", blob)

	err := d.applySnapshot(context.Background(), root, "snapshot-3", 7, false)
	if err == nil || !strings.Contains(err.Error(), "snapshot entry") {
		t.Fatalf("expected snapshot path rejection, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root.TargetAbsPath), "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("malicious snapshot touched outside path, err=%v", err)
	}
}

func TestApplySnapshotRejectsOversizedChunkMetadata(t *testing.T) {
	d, root, cancel := newSecurityTestDaemon(t)
	defer cancel()

	snapshot := protocol.SnapshotPayload{
		RootID:      root.RootID,
		Kind:        root.Kind,
		HomeRelPath: root.HomeRelPath,
		BaseSeq:     7,
		Entries: []protocol.SnapshotEntry{
			{
				Path:          "note.txt",
				Kind:          protocol.RootKindFile,
				Mode:          0o644,
				SizeBytes:     uploader.ChunkSize + 1,
				ContentSHA256: "abc",
				Chunks: []protocol.ChunkRef{
					{ObjectID: "chunk-1", PlainSize: uploader.ChunkSize + 1},
				},
			},
		},
	}
	blob := mustSnapshotBlob(t, d.keys, d.cfg.WorkspaceID, root.RootID, 7, snapshot)
	d.conn = newStaticObjectClient("snapshot-2", blob)

	err := d.applySnapshot(context.Background(), root, "snapshot-2", 7, false)
	if err == nil || !strings.Contains(err.Error(), "allowed size limits") {
		t.Fatalf("expected chunk limit rejection, got %v", err)
	}
}

func TestApplyRemoteFilePutRejectsOversizedObjectBody(t *testing.T) {
	d, root, cancel := newSecurityTestDaemon(t)
	defer cancel()
	if err := os.MkdirAll(root.TargetAbsPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}

	plain := []byte("ok")
	relPath := "note.txt"
	pathID := commoncrypto.PathID(d.keys, root.RootID, relPath)
	blob, err := commoncrypto.Encrypt(d.keys.BlobKey, plain, commoncrypto.BlobAAD(d.cfg.WorkspaceID, root.RootID, pathID, 0, int64(len(plain))))
	if err != nil {
		t.Fatalf("Encrypt chunk: %v", err)
	}
	oversized := append(append([]byte(nil), blob...), 0)
	d.conn = newStaticObjectClient("chunk-oversized", oversized)
	event := mustDaemonEvent(t, d.keys, d.cfg.WorkspaceID, root.RootID, pathID, protocol.EventFilePut, protocol.FilePutPayload{
		Path:          relPath,
		Mode:          0o644,
		MTimeNS:       time.Now().UTC().UnixNano(),
		SizeBytes:     int64(len(plain)),
		ContentSHA256: sha256Hex(plain),
		Chunks: []protocol.ChunkRef{
			{ObjectID: "chunk-oversized", PlainSize: int64(len(plain))},
		},
	})

	err = d.applyRemoteEvent(context.Background(), event)
	if err == nil || !strings.Contains(err.Error(), "object exceeds") {
		t.Fatalf("expected oversized object rejection, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root.TargetAbsPath, relPath)); !os.IsNotExist(err) {
		t.Fatalf("oversized file_put materialized target, err=%v", err)
	}
}

func TestWriteConflictCopyRejectsSymlinkAncestor(t *testing.T) {
	d, root, cancel := newSecurityTestDaemon(t)
	defer cancel()
	if err := os.MkdirAll(root.TargetAbsPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("MkdirAll(outside): %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root.TargetAbsPath, "sub")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	staged := filepath.Join(t.TempDir(), "staged.txt")
	if err := os.WriteFile(staged, []byte("local conflict\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(staged): %v", err)
	}

	_, _, _, err := d.writeConflictCopy(root, scanner.Entry{
		RelPath:       "sub/note.txt",
		Kind:          protocol.RootKindFile,
		ContentSHA256: sha256Hex([]byte("local conflict\n")),
		Mode:          0o644,
		MTimeNS:       time.Now().UTC().UnixNano(),
	}, staged, time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink ancestor rejection, got %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(outside, "*syna-conflict*"))
	if err != nil {
		t.Fatalf("Glob(outside): %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("conflict copy escaped through symlink: %v", matches)
	}
}

func TestConnectRejectsInsecureTransportByDefault(t *testing.T) {
	d, cancel := newTestDaemon(t)
	defer cancel()
	t.Setenv(insecureTransportEnv, "false")

	_, err := d.Connect(context.Background(), ConnectRequest{ServerURL: "http://example.com"})
	if err == nil || !strings.Contains(err.Error(), "insecure http transport") {
		t.Fatalf("expected insecure transport refusal, got %v", err)
	}
}

func newSecurityTestDaemon(t *testing.T) (*Daemon, state.Root, context.CancelFunc) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	keys, err := commoncrypto.Derive(bytes.Repeat([]byte{0x41}, 32))
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	d.keys = keys
	d.cfg.WorkspaceID = "workspace-1"
	root := state.Root{
		RootID:        commoncrypto.RootID(keys, "notes"),
		Kind:          protocol.RootKindDir,
		HomeRelPath:   "notes",
		TargetAbsPath: filepath.Join(home, "notes"),
		State:         protocol.RootStateActive,
	}
	if err := d.stateDB.UpsertRoot(root); err != nil {
		t.Fatalf("UpsertRoot: %v", err)
	}
	return d, root, cancel
}

func newStaticObjectClient(objectID string, blob []byte) *connector.Client {
	return &connector.Client{
		BaseURL: "https://example.test",
		Token:   "token",
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/"+objectID) {
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(strings.NewReader(`{"code":"not_found"}`)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(blob)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})},
	}
}

func newBootstrapObjectClient(t *testing.T, bootstrap protocol.BootstrapResponse, events protocol.EventFetchResponse) *connector.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/v1/bootstrap":
			_ = json.NewEncoder(w).Encode(bootstrap)
		case req.Method == http.MethodGet && req.URL.Path == "/v1/events":
			_ = json.NewEncoder(w).Encode(events)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(server.Close)
	return connector.New(server.URL).WithToken("token")
}

func mustDaemonEvent(t *testing.T, keys *commoncrypto.DerivedKeys, workspaceID, rootID, pathID string, eventType protocol.EventType, payload any) protocol.EventRecord {
	t.Helper()
	plain, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal event payload: %v", err)
	}
	blob, err := commoncrypto.Encrypt(keys.EventKey, plain, commoncrypto.EventAAD(workspaceID, rootID, pathID, string(eventType)))
	if err != nil {
		t.Fatalf("Encrypt event payload: %v", err)
	}
	event := protocol.EventRecord{
		Seq:         12,
		RootID:      rootID,
		EventType:   eventType,
		PayloadBlob: commoncrypto.Base64Raw(blob),
		CreatedAt:   time.Now().UTC(),
	}
	if pathID != "" {
		event.PathID = &pathID
	}
	return event
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func mustBootstrapRoot(t *testing.T, keys *commoncrypto.DerivedKeys, workspaceID, rootID string, payload protocol.RootAddPayload) protocol.BootstrapRoot {
	t.Helper()
	plain, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal root payload: %v", err)
	}
	blob, err := commoncrypto.Encrypt(keys.EventKey, plain, commoncrypto.EventAAD(workspaceID, rootID, "", string(protocol.EventRootAdd)))
	if err != nil {
		t.Fatalf("Encrypt root payload: %v", err)
	}
	return protocol.BootstrapRoot{
		RootID:         rootID,
		Kind:           payload.Kind,
		DescriptorBlob: commoncrypto.Base64Raw(blob),
	}
}

func mustSnapshotBlob(t *testing.T, keys *commoncrypto.DerivedKeys, workspaceID, rootID string, baseSeq int64, payload protocol.SnapshotPayload) []byte {
	t.Helper()
	plain, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal snapshot: %v", err)
	}
	blob, err := commoncrypto.Encrypt(keys.SnapshotKey, plain, commoncrypto.SnapshotAAD(workspaceID, rootID, baseSeq))
	if err != nil {
		t.Fatalf("Encrypt snapshot: %v", err)
	}
	return blob
}
