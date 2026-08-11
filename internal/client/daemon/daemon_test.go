package daemon

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"syna/internal/client/configstore"
	"syna/internal/client/connector"
	"syna/internal/client/state"
	commoncrypto "syna/internal/common/crypto"
	"syna/internal/common/protocol"
)

func TestMergeRescanHints(t *testing.T) {
	cases := []struct {
		current string
		next    string
		want    string
	}{
		{current: "a/b", next: "a/c", want: "a"},
		{current: "a/b", next: "a/b/c", want: "a/b"},
		{current: "", next: "a/b", want: ""},
		{current: "a/b", next: "", want: ""},
		{current: "x", next: "y", want: ""},
	}
	for _, tc := range cases {
		if got := mergeRescanHints(tc.current, tc.next); got != tc.want {
			t.Fatalf("mergeRescanHints(%q, %q) = %q want %q", tc.current, tc.next, got, tc.want)
		}
	}
}

func TestHandlePurgedWorkspaceClearsSyncStateButRetainsKey(t *testing.T) {
	d, cancel := newTestDaemon(t)
	defer cancel()
	rootPath := filepath.Join(t.TempDir(), "kept-files")
	if err := os.MkdirAll(rootPath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	keptFile := filepath.Join(rootPath, "notes.txt")
	if err := os.WriteFile(keptFile, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	d.cfg.ServerURL = "https://syna.example"
	d.cfg.WorkspaceID = strings.Repeat("a", 32)
	d.keyring = configstore.Keyring{
		ServerURL:    d.cfg.ServerURL,
		WorkspaceID:  d.cfg.WorkspaceID,
		WorkspaceKey: "syna1-retained",
	}
	if err := d.configs.SaveConfig(d.cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if err := d.configs.SaveKeyring(d.keyring); err != nil {
		t.Fatalf("SaveKeyring: %v", err)
	}
	if err := d.stateDB.UpsertRoot(state.Root{
		RootID:        "root-1",
		Kind:          protocol.RootKindDir,
		HomeRelPath:   "kept-files",
		TargetAbsPath: rootPath,
		State:         protocol.RootStateActive,
	}); err != nil {
		t.Fatalf("UpsertRoot: %v", err)
	}

	if err := d.handlePurgedWorkspace(); err != nil {
		t.Fatalf("handlePurgedWorkspace: %v", err)
	}
	if body, err := os.ReadFile(keptFile); err != nil || string(body) != "keep me" {
		t.Fatalf("local file changed: body=%q err=%v", body, err)
	}
	keyring, err := d.configs.LoadKeyring()
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if keyring.WorkspaceKey != "syna1-retained" {
		t.Fatalf("recovery key = %q", keyring.WorkspaceKey)
	}
	status, err := d.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Connection != protocol.ConnectionWorkspacePurged || status.LastErrorKind != protocol.IssueWorkspaceGone {
		t.Fatalf("unexpected purged status: %+v", status)
	}
	if len(status.TrackedRoots) != 0 || status.ServerURL != "" || status.WorkspaceID != "" {
		t.Fatalf("sync association was retained: %+v", status)
	}
}

func TestHandlePurgedWorkspaceRetriesAfterConfigPersistenceFailure(t *testing.T) {
	d, cancel := newTestDaemon(t)
	defer cancel()
	d.cfg.ServerURL = "https://syna.example"
	d.cfg.WorkspaceID = strings.Repeat("a", 32)
	if err := d.configs.SaveConfig(d.cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	backup := d.paths.ConfigFile + ".backup"
	if err := os.Rename(d.paths.ConfigFile, backup); err != nil {
		t.Fatalf("rename config: %v", err)
	}
	if err := os.Mkdir(d.paths.ConfigFile, 0o700); err != nil {
		t.Fatalf("mkdir at config path: %v", err)
	}

	if err := d.handlePurgedWorkspace(); err == nil {
		t.Fatal("expected config persistence failure")
	}
	workspaceState, err := d.stateDB.LoadWorkspaceState()
	if err != nil {
		t.Fatalf("LoadWorkspaceState: %v", err)
	}
	if workspaceState.ConnectionState != protocol.ConnectionWorkspacePurged {
		t.Fatalf("terminal state was not durably recorded: %+v", workspaceState)
	}
	if d.cfg.ServerURL == "" || d.cfg.WorkspaceID == "" {
		t.Fatalf("in-memory config changed before persistence succeeded: %+v", d.cfg)
	}
	if err := os.Remove(d.paths.ConfigFile); err != nil {
		t.Fatalf("remove blocking config directory: %v", err)
	}
	if err := os.Rename(backup, d.paths.ConfigFile); err != nil {
		t.Fatalf("restore config: %v", err)
	}
	if err := d.handlePurgedWorkspace(); err != nil {
		t.Fatalf("retry handlePurgedWorkspace: %v", err)
	}
	if d.cfg.ServerURL != "" || d.cfg.WorkspaceID != "" {
		t.Fatalf("config was not cleared on retry: %+v", d.cfg)
	}
}

func TestDiscardRejectedCachedSessionOnlyRetriesCachedToken(t *testing.T) {
	d, cancel := newTestDaemon(t)
	defer cancel()
	unauthorized := &connector.HTTPError{StatusCode: http.StatusUnauthorized, Code: "unauthorized"}
	workspaceState, err := d.stateDB.LoadWorkspaceState()
	if err != nil {
		t.Fatalf("LoadWorkspaceState: %v", err)
	}
	workspaceState.SessionToken = "cached-token"
	workspaceState.SessionExpiresAt = time.Now().Add(time.Hour)
	if err := d.stateDB.SaveWorkspaceState(workspaceState); err != nil {
		t.Fatalf("SaveWorkspaceState: %v", err)
	}
	discarded, err := d.discardRejectedCachedSession(unauthorized)
	if err != nil || !discarded {
		t.Fatalf("discard cached session = %t err=%v", discarded, err)
	}
	discarded, err = d.discardRejectedCachedSession(unauthorized)
	if err != nil || discarded {
		t.Fatalf("fresh auth 401 should not fast-retry: discarded=%t err=%v", discarded, err)
	}
}

func TestNewFinishesInterruptedPurgedWorkspaceTransition(t *testing.T) {
	d, cancel := newTestDaemon(t)
	defer cancel()
	d.cfg.ServerURL = "https://syna.example"
	d.cfg.WorkspaceID = strings.Repeat("a", 32)
	if err := d.configs.SaveConfig(d.cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if err := d.stateDB.ClearPurgedWorkspace(); err != nil {
		t.Fatalf("ClearPurgedWorkspace: %v", err)
	}
	paths := d.paths
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	recovered, err := New(paths, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New after interrupted transition: %v", err)
	}
	defer recovered.Close()
	if recovered.cfg.ServerURL != "" || recovered.cfg.WorkspaceID != "" || recovered.conn != nil {
		t.Fatalf("purged config was not reconciled: %+v", recovered.cfg)
	}
}

func TestConnectRejectsWhilePurgedTransitionIsPending(t *testing.T) {
	d, cancel := newTestDaemon(t)
	defer cancel()
	d.purgeTransition = true
	_, err := d.Connect(context.Background(), ConnectRequest{
		ServerURL:   "https://syna.example",
		RecoveryKey: "syna1-unused",
		Recreate:    true,
	})
	if err == nil || !strings.Contains(err.Error(), "still being finalized") {
		t.Fatalf("Connect error = %v", err)
	}
}

func TestFilterEntriesByHint(t *testing.T) {
	entries := map[string]state.Entry{
		"":           {RelPath: ""},
		"a":          {RelPath: "a"},
		"a/file":     {RelPath: "a/file"},
		"a/sub/x":    {RelPath: "a/sub/x"},
		"other/file": {RelPath: "other/file"},
	}
	filtered := filterEntriesByHint(entries, "a")
	if len(filtered) != 3 {
		t.Fatalf("unexpected filtered count %d", len(filtered))
	}
	for _, relPath := range []string{"a", "a/file", "a/sub/x"} {
		if _, ok := filtered[relPath]; !ok {
			t.Fatalf("expected %q in filtered set", relPath)
		}
	}
	if _, ok := filtered[""]; ok {
		t.Fatalf("did not expect root entry in filtered set")
	}
	if _, ok := filtered["other/file"]; ok {
		t.Fatalf("did not expect unrelated entry in filtered set")
	}
}

func TestConsumeNoOpLifecycleEventAdvancesCursor(t *testing.T) {
	d, cancel := newTestDaemon(t)
	defer cancel()

	if err := d.consumeRemoteEvent(context.Background(), protocol.EventRecord{
		Seq:       9,
		RootID:    "unknown-root",
		EventType: protocol.EventRootRemove,
	}); err != nil {
		t.Fatalf("consumeRemoteEvent: %v", err)
	}
	st, err := d.stateDB.LoadWorkspaceState()
	if err != nil {
		t.Fatalf("LoadWorkspaceState: %v", err)
	}
	if st.LastServerSeq != 9 {
		t.Fatalf("last server seq = %d want 9", st.LastServerSeq)
	}
}

func TestConsumeOwnEventAdvancesCursorWithoutApplying(t *testing.T) {
	d, cancel := newTestDaemon(t)
	defer cancel()

	root := state.Root{
		RootID:        "root-test",
		Kind:          protocol.RootKindDir,
		HomeRelPath:   "test",
		TargetAbsPath: filepath.Join(t.TempDir(), "test"),
		State:         protocol.RootStateActive,
	}
	if err := d.stateDB.UpsertRoot(root); err != nil {
		t.Fatalf("UpsertRoot: %v", err)
	}
	pathID := "path-test"
	if err := d.stateDB.UpsertEntry(state.Entry{
		RootID:     root.RootID,
		RelPath:    "file.txt",
		PathID:     pathID,
		Kind:       protocol.RootKindFile,
		CurrentSeq: 12,
	}); err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}

	if err := d.consumeRemoteEvent(context.Background(), protocol.EventRecord{
		Seq:            12,
		RootID:         root.RootID,
		PathID:         &pathID,
		EventType:      protocol.EventFilePut,
		AuthorDeviceID: d.cfg.DeviceID,
		PayloadBlob:    "deliberately-invalid",
	}); err != nil {
		t.Fatalf("consumeRemoteEvent own event: %v", err)
	}
	st, err := d.stateDB.LoadWorkspaceState()
	if err != nil {
		t.Fatalf("LoadWorkspaceState: %v", err)
	}
	if st.LastServerSeq != 12 {
		t.Fatalf("last server seq = %d want 12", st.LastServerSeq)
	}
}

func TestConsumeOwnRootRemoveAppliesWhenRootStillActive(t *testing.T) {
	d, cancel := newTestDaemon(t)
	defer cancel()

	root := state.Root{
		RootID:        "root-test",
		Kind:          protocol.RootKindDir,
		HomeRelPath:   "test",
		TargetAbsPath: filepath.Join(t.TempDir(), "test"),
		State:         protocol.RootStateActive,
	}
	if err := d.stateDB.UpsertRoot(root); err != nil {
		t.Fatalf("UpsertRoot: %v", err)
	}
	if err := d.consumeRemoteEvent(context.Background(), protocol.EventRecord{
		Seq:            13,
		RootID:         root.RootID,
		EventType:      protocol.EventRootRemove,
		AuthorDeviceID: d.cfg.DeviceID,
	}); err != nil {
		t.Fatalf("consumeRemoteEvent own root_remove: %v", err)
	}
	got, err := d.stateDB.RootByID(root.RootID)
	if err != nil {
		t.Fatalf("RootByID: %v", err)
	}
	if got.State != protocol.RootStateRemoved {
		t.Fatalf("root state = %s want %s", got.State, protocol.RootStateRemoved)
	}
	st, err := d.stateDB.LoadWorkspaceState()
	if err != nil {
		t.Fatalf("LoadWorkspaceState: %v", err)
	}
	if st.LastServerSeq != 13 {
		t.Fatalf("last server seq = %d want 13", st.LastServerSeq)
	}
}

func TestConsumeOwnRootAddReactivatesRemovedRoot(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()

	keys, err := commoncrypto.Derive(make([]byte, 32))
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	d.keys = keys
	d.cfg.WorkspaceID = "workspace-test"
	d.cfg.DeviceID = "device-test"
	homeRelPath := "notes"
	rootID := commoncrypto.RootID(keys, homeRelPath)
	target := filepath.Join(home, homeRelPath)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	if err := d.stateDB.UpsertRoot(state.Root{
		RootID:        rootID,
		Kind:          protocol.RootKindDir,
		HomeRelPath:   homeRelPath,
		TargetAbsPath: target,
		State:         protocol.RootStateRemoved,
	}); err != nil {
		t.Fatalf("UpsertRoot: %v", err)
	}

	event := mustDaemonEvent(t, keys, d.cfg.WorkspaceID, rootID, "", protocol.EventRootAdd, protocol.RootAddPayload{
		RootID:      rootID,
		Kind:        protocol.RootKindDir,
		HomeRelPath: homeRelPath,
	})
	event.AuthorDeviceID = d.cfg.DeviceID
	if err := d.consumeRemoteEvent(context.Background(), event); err != nil {
		t.Fatalf("consumeRemoteEvent own root_add: %v", err)
	}

	root, err := d.stateDB.RootByID(rootID)
	if err != nil {
		t.Fatalf("RootByID: %v", err)
	}
	if root.State != protocol.RootStateActive {
		t.Fatalf("root state = %s want %s", root.State, protocol.RootStateActive)
	}
	st, err := d.stateDB.LoadWorkspaceState()
	if err != nil {
		t.Fatalf("LoadWorkspaceState: %v", err)
	}
	if st.LastServerSeq != event.Seq {
		t.Fatalf("last server seq = %d want %d", st.LastServerSeq, event.Seq)
	}
}

func TestActiveRootAfterInitialSyncRejectsReplacement(t *testing.T) {
	d, cancel := newTestDaemon(t)
	defer cancel()
	if err := d.stateDB.UpsertRoot(state.Root{
		RootID:      "root-test",
		Kind:        protocol.RootKindDir,
		State:       protocol.RootStateActive,
		HomeRelPath: "notes",
	}); err != nil {
		t.Fatalf("UpsertRoot: %v", err)
	}
	d.conn = newBootstrapObjectClient(t, protocol.BootstrapResponse{
		Roots: []protocol.BootstrapRoot{{RootID: "root-test", CreatedSeq: 17}},
	}, protocol.EventFetchResponse{})

	matched, active, err := d.verifyRootIncarnation(context.Background(), "root-test", 11)
	if err != nil {
		t.Fatalf("verifyRootIncarnation: %v", err)
	}
	if matched || !active {
		t.Fatalf("verifyRootIncarnation = matched %t active %t, want false true", matched, active)
	}
}

func TestConsumeOwnPostSnapshotEventReplaysWhenMissingLocally(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()

	keys, err := commoncrypto.Derive(make([]byte, 32))
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	d.keys = keys
	d.cfg.WorkspaceID = "workspace-test"
	d.cfg.DeviceID = "device-test"
	root := state.Root{
		RootID:            commoncrypto.RootID(keys, "notes"),
		Kind:              protocol.RootKindDir,
		HomeRelPath:       "notes",
		TargetAbsPath:     filepath.Join(home, "notes"),
		State:             protocol.RootStateActive,
		LatestSnapshotSeq: 5,
	}
	if err := os.MkdirAll(root.TargetAbsPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	if err := d.stateDB.UpsertRoot(root); err != nil {
		t.Fatalf("UpsertRoot: %v", err)
	}
	payload := protocol.DirPutPayload{Path: "post-snapshot", Mode: 0o755, MTimeNS: 1}
	pathID := commoncrypto.PathID(keys, root.RootID, payload.Path)
	event := mustDaemonEvent(t, keys, d.cfg.WorkspaceID, root.RootID, pathID, protocol.EventDirPut, payload)
	event.Seq = 6
	event.AuthorDeviceID = d.cfg.DeviceID
	if err := d.consumeRemoteEvent(context.Background(), event); err != nil {
		t.Fatalf("consumeRemoteEvent own post-snapshot event: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root.TargetAbsPath, payload.Path)); err != nil {
		t.Fatalf("post-snapshot directory was not materialized: %v", err)
	}
}

func TestRootForRemoveReportsUntrackedPath(t *testing.T) {
	roots := []state.Root{
		{
			RootID:        "root-test",
			Kind:          protocol.RootKindDir,
			HomeRelPath:   "test",
			TargetAbsPath: "/home/trickster/test",
			State:         protocol.RootStateActive,
		},
	}
	_, err := rootForRemove(roots, "/home/trickster/Coding/syna", "Coding/syna")
	if err == nil {
		t.Fatalf("expected untracked path error")
	}
	if strings.Contains(err.Error(), "sql: no rows") {
		t.Fatalf("leaked sql no rows error: %v", err)
	}
	if !strings.Contains(err.Error(), "not a tracked root") || !strings.Contains(err.Error(), "/home/trickster/test") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRootForRemoveReportsPathInsideTrackedRoot(t *testing.T) {
	roots := []state.Root{
		{
			RootID:        "root-test",
			Kind:          protocol.RootKindDir,
			HomeRelPath:   "test",
			TargetAbsPath: "/home/trickster/test",
			State:         protocol.RootStateActive,
		},
	}
	_, err := rootForRemove(roots, "/home/trickster/test/nested", "test/nested")
	if err == nil {
		t.Fatalf("expected inside tracked root error")
	}
	if !strings.Contains(err.Error(), "inside tracked root") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateServerURLRequiresHost(t *testing.T) {
	if err := validateServerURL("https:///missing-host"); err == nil || !strings.Contains(err.Error(), "must include a host") {
		t.Fatalf("expected missing host rejection, got %v", err)
	}
}

func TestRemoveRootRejectsWrongDirectoryWithoutChangingTrackedRoot(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()

	trackedDir := filepath.Join(home, "test")
	wrongDir := filepath.Join(home, "Coding", "syna")
	if err := os.MkdirAll(trackedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(tracked): %v", err)
	}
	if err := os.MkdirAll(wrongDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(wrong): %v", err)
	}
	if err := d.stateDB.UpsertRoot(state.Root{
		RootID:        "root-test",
		Kind:          protocol.RootKindDir,
		HomeRelPath:   "test",
		TargetAbsPath: trackedDir,
		State:         protocol.RootStateActive,
	}); err != nil {
		t.Fatalf("UpsertRoot: %v", err)
	}
	d.conn = connector.New("http://127.0.0.1:1")
	d.keys = &commoncrypto.DerivedKeys{}

	err := d.RemoveRoot(context.Background(), wrongDir)
	if err == nil {
		t.Fatalf("expected wrong directory to be rejected")
	}
	if !strings.Contains(err.Error(), "not a tracked root") {
		t.Fatalf("unexpected error: %v", err)
	}
	root, err := d.stateDB.RootByHomeRel("test")
	if err != nil {
		t.Fatalf("RootByHomeRel: %v", err)
	}
	if root.State != protocol.RootStateActive {
		t.Fatalf("wrong directory removed tracked root, state=%s", root.State)
	}
}
