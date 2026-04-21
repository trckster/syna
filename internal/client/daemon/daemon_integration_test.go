package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	commoncfg "syna/internal/common/config"
	"syna/internal/common/protocol"
	"syna/internal/server/api"
	servercfg "syna/internal/server/config"
	"syna/internal/server/db"
	"syna/internal/server/hub"
	"syna/internal/server/objectstore"
)

type integrationHarness struct {
	serverURL string
	dataDir   string
	serverDB  *db.DB
	closeFn   func()
}

type restartableHarness struct {
	serverURL string
	dataDir   string
	serverDB  *db.DB
	handler   http.Handler
	addr      string
	server    *http.Server
}

func newIntegrationHarness(t *testing.T) *integrationHarness {
	t.Helper()

	dataDir := filepath.Join(t.TempDir(), "server")
	if err := servercfg.EnsureDataDirs(dataDir); err != nil {
		t.Fatalf("EnsureDataDirs: %v", err)
	}
	database, err := db.Open(filepath.Join(dataDir, "state.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := database.Migrate(); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	cfg := servercfg.Config{
		DataDir:           dataDir,
		SessionTTL:        time.Hour,
		EventRetention:    24 * time.Hour,
		ZeroRefRetention:  24 * time.Hour,
		AllowHTTP:         true,
		MaxEventFetchPage: 1000,
		MaxPlainChunkSize: 4 << 20,
		MaxEventBodyBytes: 1 << 20,
		MaxSnapshotBody:   16 << 20,
		MaxSnapshotPlain:  16 << 20,
		MaxWSClients:      8,
	}
	server := httptest.NewServer(api.New(cfg, database, objectstore.New(dataDir), hub.New(cfg.MaxWSClients, log.New(io.Discard, "", 0)), log.New(io.Discard, "", 0)).Handler())
	return &integrationHarness{
		serverURL: server.URL,
		dataDir:   dataDir,
		serverDB:  database,
		closeFn: func() {
			server.Close()
			_ = database.Close()
		},
	}
}

func (h *integrationHarness) Close() {
	if h != nil && h.closeFn != nil {
		h.closeFn()
	}
}

func newRestartableHarness(t *testing.T) *restartableHarness {
	t.Helper()

	dataDir := filepath.Join(t.TempDir(), "server")
	if err := servercfg.EnsureDataDirs(dataDir); err != nil {
		t.Fatalf("EnsureDataDirs: %v", err)
	}
	database, err := db.Open(filepath.Join(dataDir, "state.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := database.Migrate(); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	cfg := servercfg.Config{
		DataDir:           dataDir,
		SessionTTL:        time.Hour,
		EventRetention:    24 * time.Hour,
		ZeroRefRetention:  24 * time.Hour,
		AllowHTTP:         true,
		MaxEventFetchPage: 1000,
		MaxPlainChunkSize: 4 << 20,
		MaxEventBodyBytes: 1 << 20,
		MaxSnapshotBody:   16 << 20,
		MaxSnapshotPlain:  16 << 20,
		MaxWSClients:      8,
	}
	h := &restartableHarness{
		dataDir:  dataDir,
		serverDB: database,
		handler:  api.New(cfg, database, objectstore.New(dataDir), hub.New(cfg.MaxWSClients, log.New(io.Discard, "", 0)), log.New(io.Discard, "", 0)).Handler(),
	}
	h.Start(t)
	return h
}

func (h *restartableHarness) Start(t *testing.T) {
	t.Helper()
	addr := h.addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	var (
		ln  net.Listener
		err error
	)
	for attempt := 0; attempt < 20; attempt++ {
		ln, err = net.Listen("tcp", addr)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Listen(%s): %v", addr, err)
	}
	h.addr = ln.Addr().String()
	h.serverURL = "http://" + h.addr
	h.server = &http.Server{Handler: h.handler}
	go func() {
		if err := h.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("restartable server stopped: %v", err)
		}
	}()
}

func (h *restartableHarness) Stop() {
	if h != nil && h.server != nil {
		_ = h.server.Close()
		h.server = nil
	}
}

func (h *restartableHarness) Close() {
	if h == nil {
		return
	}
	h.Stop()
	_ = h.serverDB.Close()
}

func newTestDaemon(t *testing.T) (*Daemon, context.CancelFunc) {
	t.Helper()
	t.Setenv(insecureTransportEnv, "true")

	baseDir := t.TempDir()
	paths := commoncfg.ClientPaths{
		ConfigDir:   filepath.Join(baseDir, "config"),
		StateDir:    filepath.Join(baseDir, "state"),
		ConfigFile:  filepath.Join(baseDir, "config", "config.json"),
		KeyringFile: filepath.Join(baseDir, "config", "keyring.json"),
		DBFile:      filepath.Join(baseDir, "state", "client.db"),
		SocketFile:  filepath.Join(baseDir, "state", "agent.sock"),
		PIDFile:     filepath.Join(baseDir, "state", "daemon.pid"),
		SystemdDir:  filepath.Join(baseDir, "config", "systemd", "user"),
		UnitFile:    filepath.Join(baseDir, "config", "systemd", "user", "syna.service"),
	}
	d, err := New(paths, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New daemon: %v", err)
	}
	d.cfg.DaemonAutoStart = false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.runCtx = ctx
	d.shutdown = cancel
	t.Cleanup(func() {
		cancel()
		_ = d.Close()
	})
	return d, cancel
}

func setHome(t *testing.T, home string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("MkdirAll(home): %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
}

func serverDataContains(t *testing.T, dataDir, needle string) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(dataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || found || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), needle) {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", dataDir, err)
	}
	return found
}

func TestIntegrationCreateFreshWorkspace(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	d, cancel := newTestDaemon(t)
	defer cancel()

	resp, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if resp.WorkspaceID == "" {
		t.Fatalf("expected workspace ID")
	}
	if resp.GeneratedRecoveryKey == "" {
		t.Fatalf("expected generated recovery key for fresh workspace")
	}

	st, err := d.stateDB.LoadWorkspaceState()
	if err != nil {
		t.Fatalf("LoadWorkspaceState: %v", err)
	}
	if st.ServerURL != h.serverURL {
		t.Fatalf("server URL mismatch: got %q want %q", st.ServerURL, h.serverURL)
	}
	if st.WorkspaceID != resp.WorkspaceID {
		t.Fatalf("workspace ID mismatch: got %q want %q", st.WorkspaceID, resp.WorkspaceID)
	}
}

func TestIntegrationJoinExistingWorkspaceWithRecoveryKey(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	first, cancelFirst := newTestDaemon(t)
	defer cancelFirst()
	firstResp, err := first.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}

	second, cancelSecond := newTestDaemon(t)
	defer cancelSecond()
	secondResp, err := second.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: firstResp.GeneratedRecoveryKey,
	})
	if err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	if secondResp.WorkspaceID != firstResp.WorkspaceID {
		t.Fatalf("workspace IDs differ: got %q want %q", secondResp.WorkspaceID, firstResp.WorkspaceID)
	}
	if secondResp.GeneratedRecoveryKey != "" {
		t.Fatalf("join should not generate a new recovery key")
	}
}

func TestIntegrationJoinBootstrapsExistingWorkspaceDuringConnect(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home1 := filepath.Join(t.TempDir(), "home-one")
	setHome(t, home1)
	first, cancelFirst := newTestDaemon(t)
	defer cancelFirst()
	firstResp, err := first.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}

	rootDir1 := filepath.Join(home1, "notes")
	if err := os.MkdirAll(filepath.Join(rootDir1, "deep"), 0o755); err != nil {
		t.Fatalf("MkdirAll(root1): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir1, "deep", "note.txt"), []byte("restored\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(root1): %v", err)
	}
	if err := first.AddRoot(context.Background(), rootDir1); err != nil {
		t.Fatalf("first AddRoot: %v", err)
	}

	home2 := filepath.Join(t.TempDir(), "home-two")
	setHome(t, home2)
	second, cancelSecond := newTestDaemon(t)
	defer cancelSecond()
	if _, err := second.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: firstResp.GeneratedRecoveryKey,
	}); err != nil {
		t.Fatalf("second Connect: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(home2, "notes", "deep", "note.txt"))
	if err != nil {
		t.Fatalf("ReadFile(restored): %v", err)
	}
	if string(got) != "restored\n" {
		t.Fatalf("unexpected restored contents %q", string(got))
	}
	st, err := second.stateDB.LoadWorkspaceState()
	if err != nil {
		t.Fatalf("LoadWorkspaceState(second): %v", err)
	}
	if st.LastServerSeq == 0 {
		t.Fatalf("expected connect bootstrap to advance last server sequence")
	}
}

func TestIntegrationReplayOwnRootAddKeepsRootActive(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	rootDir := filepath.Join(home, "notes")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "note.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(root): %v", err)
	}
	if err := d.AddRoot(context.Background(), rootDir); err != nil {
		t.Fatalf("AddRoot: %v", err)
	}

	bootstrap, err := d.conn.Bootstrap(context.Background())
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	var rootAdd *protocol.EventRecord
	for _, root := range bootstrap.Roots {
		rootAdd = &protocol.EventRecord{
			Seq:         root.CreatedSeq,
			RootID:      root.RootID,
			EventType:   protocol.EventRootAdd,
			PayloadBlob: root.DescriptorBlob,
		}
		break
	}
	if rootAdd == nil {
		t.Fatalf("expected root_add event")
	}
	if err := d.applyRemoteEvent(context.Background(), *rootAdd); err != nil {
		t.Fatalf("apply own root_add: %v", err)
	}
	root, err := d.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel: %v", err)
	}
	if root.State != protocol.RootStateActive {
		t.Fatalf("own root_add replay changed root state to %q", root.State)
	}
}

func TestIntegrationStatusSurfacesScannerWarnings(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	rootDir := filepath.Join(home, "notes")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	if err := os.Symlink("/tmp", filepath.Join(rootDir, "external")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := d.AddRoot(context.Background(), rootDir); err != nil {
		t.Fatalf("AddRoot: %v", err)
	}
	status, err := d.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(status.Warnings) != 1 || !strings.Contains(status.Warnings[0], "ignored symlink: external") {
		t.Fatalf("expected scanner warning in status, got %+v", status.Warnings)
	}
	if len(status.Issues) != 1 || status.Issues[0].Kind != protocol.IssueScanner {
		t.Fatalf("expected structured scanner issue, got %+v", status.Issues)
	}
}

func TestIntegrationDeletedWatchedDirectoryBecomesRemovedRoot(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	rootDir := filepath.Join(home, "notes")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "note.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(note): %v", err)
	}
	if err := d.AddRoot(context.Background(), rootDir); err != nil {
		t.Fatalf("AddRoot: %v", err)
	}
	root, err := d.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel: %v", err)
	}
	if err := d.stateDB.UpsertWarning("watcher:"+root.RootID, "watcher could not monitor stale root", time.Now().UTC()); err != nil {
		t.Fatalf("UpsertWarning(watcher): %v", err)
	}
	if err := d.stateDB.UpsertWarning("scanner:"+root.RootID+":0", "notes: ignored symlink: stale", time.Now().UTC()); err != nil {
		t.Fatalf("UpsertWarning(scanner): %v", err)
	}

	if err := os.RemoveAll(rootDir); err != nil {
		t.Fatalf("RemoveAll(root): %v", err)
	}
	if err := d.rescanRootHint(context.Background(), root.RootID, "note.txt"); err != nil {
		t.Fatalf("rescan deleted root: %v", err)
	}

	status, err := d.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.LastError != "" || len(status.Warnings) != 0 || len(status.Issues) != 0 {
		t.Fatalf("deleted root should not leave status errors, got %+v", status)
	}
	if len(status.TrackedRoots) != 1 || status.TrackedRoots[0].State != protocol.RootStateRemoved {
		t.Fatalf("expected removed root status, got %+v", status.TrackedRoots)
	}
	bootstrap, err := d.conn.Bootstrap(context.Background())
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(bootstrap.Roots) != 0 {
		t.Fatalf("expected server root_remove to remove active roots, got %+v", bootstrap.Roots)
	}
}

func TestIntegrationServerStoresNoPlaintextContentOrPaths(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	rootDir := filepath.Join(home, "private-notes")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "secret.txt"), []byte("plain text secret\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(secret): %v", err)
	}
	if err := d.AddRoot(context.Background(), rootDir); err != nil {
		t.Fatalf("AddRoot: %v", err)
	}

	for _, forbidden := range []string{"plain text secret", "secret.txt", "private-notes", rootDir} {
		if serverDataContains(t, h.dataDir, forbidden) {
			t.Fatalf("server data contains forbidden plaintext %q", forbidden)
		}
	}
}

func TestIntegrationRejectConnectToDifferentServerBeforeDisconnect(t *testing.T) {
	firstServer := newIntegrationHarness(t)
	defer firstServer.Close()
	secondServer := newIntegrationHarness(t)
	defer secondServer.Close()

	d, cancel := newTestDaemon(t)
	defer cancel()

	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: firstServer.serverURL}); err != nil {
		t.Fatalf("initial Connect: %v", err)
	}
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: secondServer.serverURL}); err == nil {
		t.Fatalf("expected second connect to fail")
	} else if !strings.Contains(err.Error(), "already connected") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestIntegrationConcurrentEditConflictCopy(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home1 := filepath.Join(t.TempDir(), "home-one")
	setHome(t, home1)
	first, cancelFirst := newTestDaemon(t)
	defer cancelFirst()
	firstResp, err := first.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}

	rootDir1 := filepath.Join(home1, "notes")
	if err := os.MkdirAll(rootDir1, 0o755); err != nil {
		t.Fatalf("MkdirAll(root1): %v", err)
	}
	file1 := filepath.Join(rootDir1, "note.txt")
	if err := os.WriteFile(file1, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(base1): %v", err)
	}
	if err := first.AddRoot(context.Background(), rootDir1); err != nil {
		t.Fatalf("first AddRoot: %v", err)
	}

	home2 := filepath.Join(t.TempDir(), "home-two")
	setHome(t, home2)
	second, cancelSecond := newTestDaemon(t)
	defer cancelSecond()
	if _, err := second.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: firstResp.GeneratedRecoveryKey,
	}); err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	if err := second.bootstrap(context.Background()); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}

	root, err := first.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel: %v", err)
	}

	if err := os.WriteFile(file1, []byte("from-first\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(first): %v", err)
	}
	file2 := filepath.Join(home2, "notes", "note.txt")
	if err := os.WriteFile(file2, []byte("from-second\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(second): %v", err)
	}

	if err := first.rescanRootHint(context.Background(), root.RootID, "note.txt"); err != nil {
		t.Fatalf("first rescanRootHint: %v", err)
	}
	if err := second.rescanRootHint(context.Background(), root.RootID, "note.txt"); err != nil {
		t.Fatalf("second rescanRootHint: %v", err)
	}
	if err := first.bootstrapOrCatchUp(context.Background()); err != nil {
		t.Fatalf("first bootstrapOrCatchUp: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(home2, "notes", "note.syna-conflict-*"))
	if err != nil {
		t.Fatalf("Glob(second): %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected one conflict copy for second client, got %v", matches)
	}
	conflictBytes, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("ReadFile(conflict second): %v", err)
	}
	if string(conflictBytes) != "from-second\n" {
		t.Fatalf("unexpected conflict contents %q", string(conflictBytes))
	}

	replicated, err := filepath.Glob(filepath.Join(home1, "notes", "note.syna-conflict-*"))
	if err != nil {
		t.Fatalf("Glob(first): %v", err)
	}
	if len(replicated) != 1 {
		t.Fatalf("expected replicated conflict copy on first client, got %v", replicated)
	}
	entries, err := second.stateDB.EntriesForRoot(root.RootID)
	if err != nil {
		t.Fatalf("EntriesForRoot(second): %v", err)
	}
	foundConflict := false
	for relPath := range entries {
		if strings.HasPrefix(relPath, "note.syna-conflict-") {
			foundConflict = true
			break
		}
	}
	if !foundConflict {
		t.Fatalf("expected conflict entry in second client state")
	}
}

func TestIntegrationRemoteOnlyCreateRaceDoesNotCreateConflictCopy(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home1 := filepath.Join(t.TempDir(), "home-one")
	setHome(t, home1)
	first, cancelFirst := newTestDaemon(t)
	defer cancelFirst()
	firstResp, err := first.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}

	rootDir1 := filepath.Join(home1, "notes")
	if err := os.MkdirAll(rootDir1, 0o755); err != nil {
		t.Fatalf("MkdirAll(root1): %v", err)
	}
	if err := first.AddRoot(context.Background(), rootDir1); err != nil {
		t.Fatalf("first AddRoot: %v", err)
	}

	home2 := filepath.Join(t.TempDir(), "home-two")
	setHome(t, home2)
	second, cancelSecond := newTestDaemon(t)
	defer cancelSecond()
	if _, err := second.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: firstResp.GeneratedRecoveryKey,
	}); err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	if err := second.bootstrap(context.Background()); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}

	root, err := second.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel(second): %v", err)
	}
	file1 := filepath.Join(rootDir1, "file2")
	if err := os.WriteFile(file1, []byte("from-vps\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(first): %v", err)
	}
	if err := first.rescanRootHint(context.Background(), root.RootID, "file2"); err != nil {
		t.Fatalf("first rescanRootHint: %v", err)
	}
	if err := second.bootstrapOrCatchUp(context.Background()); err != nil {
		t.Fatalf("second bootstrapOrCatchUp: %v", err)
	}

	file2 := filepath.Join(home2, "notes", "file2")
	if got, err := os.ReadFile(file2); err != nil {
		t.Fatalf("ReadFile(second file2): %v", err)
	} else if string(got) != "from-vps\n" {
		t.Fatalf("unexpected second file2 contents %q", string(got))
	}

	if err := second.stateDB.DeleteEntry(root.RootID, "file2"); err != nil {
		t.Fatalf("DeleteEntry(file2): %v", err)
	}
	if _, err := second.stateDB.SQL.Exec(`DELETE FROM ignore_events WHERE root_id = ? AND rel_path = ?`, root.RootID, "file2"); err != nil {
		t.Fatalf("delete ignore event: %v", err)
	}
	if err := second.rescanRootHint(context.Background(), root.RootID, "file2"); err != nil {
		t.Fatalf("second race rescanRootHint: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(home2, "notes", "file2.syna-conflict-*"))
	if err != nil {
		t.Fatalf("Glob(conflicts): %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected no conflict copies for remote-only create, got %v", matches)
	}
	entries, err := second.stateDB.EntriesForRoot(root.RootID)
	if err != nil {
		t.Fatalf("EntriesForRoot(second): %v", err)
	}
	if _, ok := entries["file2"]; !ok {
		t.Fatalf("expected file2 entry to be restored after applying remote head")
	}
}

func TestIntegrationRecreateAfterDeleteUsesDeletedPathHead(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	rootDir := filepath.Join(home, "notes")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	if err := d.AddRoot(context.Background(), rootDir); err != nil {
		t.Fatalf("AddRoot: %v", err)
	}
	root, err := d.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel: %v", err)
	}

	note := filepath.Join(rootDir, "note.txt")
	if err := os.WriteFile(note, []byte("first\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(first): %v", err)
	}
	if err := d.rescanRootHint(context.Background(), root.RootID, "note.txt"); err != nil {
		t.Fatalf("rescan create: %v", err)
	}
	if err := os.Remove(note); err != nil {
		t.Fatalf("Remove(note): %v", err)
	}
	if err := d.rescanRootHint(context.Background(), root.RootID, "note.txt"); err != nil {
		t.Fatalf("rescan delete: %v", err)
	}
	if err := os.WriteFile(note, []byte("second\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(second): %v", err)
	}
	if err := d.rescanRootHint(context.Background(), root.RootID, "note.txt"); err != nil {
		t.Fatalf("rescan recreate: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(rootDir, "note.syna-conflict-*"))
	if err != nil {
		t.Fatalf("Glob(conflicts): %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected no conflict copy after recreate, got %v", matches)
	}
	entries, err := d.stateDB.EntriesForRoot(root.RootID)
	if err != nil {
		t.Fatalf("EntriesForRoot: %v", err)
	}
	entry, ok := entries["note.txt"]
	if !ok || entry.Deleted {
		t.Fatalf("expected active recreated entry, got %+v ok=%v", entry, ok)
	}
}

func TestIntegrationBootstrapIntoEmptyTarget(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home1 := filepath.Join(t.TempDir(), "home-one")
	setHome(t, home1)
	first, cancelFirst := newTestDaemon(t)
	defer cancelFirst()
	firstResp, err := first.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}

	rootDir1 := filepath.Join(home1, "notes")
	if err := os.MkdirAll(rootDir1, 0o755); err != nil {
		t.Fatalf("MkdirAll(root1): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir1, "note.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(root1): %v", err)
	}
	if err := first.AddRoot(context.Background(), rootDir1); err != nil {
		t.Fatalf("first AddRoot: %v", err)
	}

	home2 := filepath.Join(t.TempDir(), "home-two")
	setHome(t, home2)
	second, cancelSecond := newTestDaemon(t)
	defer cancelSecond()
	if _, err := second.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: firstResp.GeneratedRecoveryKey,
	}); err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	if err := second.bootstrap(context.Background()); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(home2, "notes", "note.txt"))
	if err != nil {
		t.Fatalf("ReadFile(bootstrapped): %v", err)
	}
	if string(got) != "hello\n" {
		t.Fatalf("unexpected bootstrapped contents %q", string(got))
	}
}

func TestIntegrationRejectBootstrapIntoNonEmptyTarget(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home1 := filepath.Join(t.TempDir(), "home-one")
	setHome(t, home1)
	first, cancelFirst := newTestDaemon(t)
	defer cancelFirst()
	firstResp, err := first.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}

	rootDir1 := filepath.Join(home1, "notes")
	if err := os.MkdirAll(rootDir1, 0o755); err != nil {
		t.Fatalf("MkdirAll(root1): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir1, "note.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(root1): %v", err)
	}
	if err := first.AddRoot(context.Background(), rootDir1); err != nil {
		t.Fatalf("first AddRoot: %v", err)
	}

	home2 := filepath.Join(t.TempDir(), "home-two")
	setHome(t, home2)
	blockedDir := filepath.Join(home2, "notes")
	if err := os.MkdirAll(blockedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(blocked): %v", err)
	}
	if err := os.WriteFile(filepath.Join(blockedDir, "local.txt"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(blocked): %v", err)
	}

	second, cancelSecond := newTestDaemon(t)
	defer cancelSecond()
	if _, err := second.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: firstResp.GeneratedRecoveryKey,
	}); err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	if err := second.bootstrap(context.Background()); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}

	root, err := second.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel(second): %v", err)
	}
	if root.State != protocol.RootStateBlockedNonEmpty {
		t.Fatalf("unexpected root state %q", root.State)
	}
	if _, err := os.Stat(filepath.Join(blockedDir, "local.txt")); err != nil {
		t.Fatalf("expected local file to remain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(blockedDir, "note.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected remote file not to be materialized, got err=%v", err)
	}
}

func TestIntegrationAddDirectoryRootWithNestedFiles(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home1 := filepath.Join(t.TempDir(), "home-one")
	setHome(t, home1)
	first, cancelFirst := newTestDaemon(t)
	defer cancelFirst()
	firstResp, err := first.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}

	rootDir1 := filepath.Join(home1, "notes")
	if err := os.MkdirAll(filepath.Join(rootDir1, "deep"), 0o755); err != nil {
		t.Fatalf("MkdirAll(root1): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir1, "top.txt"), []byte("top\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(top): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir1, "deep", "nested.txt"), []byte("nested\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(nested): %v", err)
	}
	if err := first.AddRoot(context.Background(), rootDir1); err != nil {
		t.Fatalf("first AddRoot: %v", err)
	}

	root, err := first.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel(first): %v", err)
	}
	entries, err := first.stateDB.EntriesForRoot(root.RootID)
	if err != nil {
		t.Fatalf("EntriesForRoot(first): %v", err)
	}
	for _, relPath := range []string{"", "deep", "top.txt", "deep/nested.txt"} {
		if _, ok := entries[relPath]; !ok {
			t.Fatalf("expected entry %q in first client state", relPath)
		}
	}

	home2 := filepath.Join(t.TempDir(), "home-two")
	setHome(t, home2)
	second, cancelSecond := newTestDaemon(t)
	defer cancelSecond()
	if _, err := second.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: firstResp.GeneratedRecoveryKey,
	}); err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	if err := second.bootstrap(context.Background()); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	for path, want := range map[string]string{
		filepath.Join(home2, "notes", "top.txt"):            "top\n",
		filepath.Join(home2, "notes", "deep", "nested.txt"): "nested\n",
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		if string(got) != want {
			t.Fatalf("unexpected contents for %s: %q", path, string(got))
		}
	}
}

func TestIntegrationDisconnectAndLaterReconnect(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()

	resp, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	rootDir := filepath.Join(home, "notes")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "note.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(root): %v", err)
	}
	if err := d.AddRoot(context.Background(), rootDir); err != nil {
		t.Fatalf("AddRoot: %v", err)
	}

	if err := d.Disconnect(context.Background()); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	roots, err := d.stateDB.ListRoots()
	if err != nil {
		t.Fatalf("ListRoots(after disconnect): %v", err)
	}
	if len(roots) != 0 {
		t.Fatalf("expected roots to be cleared after disconnect, got %d", len(roots))
	}

	if err := os.RemoveAll(rootDir); err != nil {
		t.Fatalf("RemoveAll(root): %v", err)
	}
	if _, err := d.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: resp.GeneratedRecoveryKey,
	}); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if err := d.bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap after reconnect: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(rootDir, "note.txt"))
	if err != nil {
		t.Fatalf("ReadFile(reconnected): %v", err)
	}
	if string(got) != "hello\n" {
		t.Fatalf("unexpected restored contents %q", string(got))
	}
}

func TestIntegrationCrossClientLiveUpdate(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home1 := filepath.Join(t.TempDir(), "home-one")
	setHome(t, home1)
	first, cancelFirst := newTestDaemon(t)
	defer cancelFirst()
	firstResp, err := first.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}

	rootDir1 := filepath.Join(home1, "notes")
	if err := os.MkdirAll(rootDir1, 0o755); err != nil {
		t.Fatalf("MkdirAll(root1): %v", err)
	}
	file1 := filepath.Join(rootDir1, "note.txt")
	if err := os.WriteFile(file1, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(root1): %v", err)
	}
	if err := first.AddRoot(context.Background(), rootDir1); err != nil {
		t.Fatalf("first AddRoot: %v", err)
	}

	root, err := first.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel(first): %v", err)
	}

	home2 := filepath.Join(t.TempDir(), "home-two")
	setHome(t, home2)
	second, cancelSecond := newTestDaemon(t)
	defer cancelSecond()
	if _, err := second.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: firstResp.GeneratedRecoveryKey,
	}); err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	if err := second.bootstrap(context.Background()); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}

	if err := os.WriteFile(file1, []byte("updated\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(updated): %v", err)
	}
	if err := first.rescanRootHint(context.Background(), root.RootID, "note.txt"); err != nil {
		t.Fatalf("first rescanRootHint: %v", err)
	}
	if err := second.bootstrapOrCatchUp(context.Background()); err != nil {
		t.Fatalf("second bootstrapOrCatchUp: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(home2, "notes", "note.txt"))
	if err != nil {
		t.Fatalf("ReadFile(second): %v", err)
	}
	if string(got) != "updated\n" {
		t.Fatalf("unexpected replicated contents %q", string(got))
	}
}

func TestIntegrationDeletePropagatesBetweenClients(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home1 := filepath.Join(t.TempDir(), "home-one")
	setHome(t, home1)
	first, cancelFirst := newTestDaemon(t)
	defer cancelFirst()
	firstResp, err := first.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}

	rootDir1 := filepath.Join(home1, "notes")
	if err := os.MkdirAll(rootDir1, 0o755); err != nil {
		t.Fatalf("MkdirAll(root1): %v", err)
	}
	file1 := filepath.Join(rootDir1, "note.txt")
	if err := os.WriteFile(file1, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(root1): %v", err)
	}
	if err := first.AddRoot(context.Background(), rootDir1); err != nil {
		t.Fatalf("first AddRoot: %v", err)
	}

	root, err := first.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel(first): %v", err)
	}

	home2 := filepath.Join(t.TempDir(), "home-two")
	setHome(t, home2)
	second, cancelSecond := newTestDaemon(t)
	defer cancelSecond()
	if _, err := second.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: firstResp.GeneratedRecoveryKey,
	}); err != nil {
		t.Fatalf("second Connect: %v", err)
	}

	if err := os.Remove(file1); err != nil {
		t.Fatalf("Remove(first): %v", err)
	}
	if err := first.rescanRootHint(context.Background(), root.RootID, "note.txt"); err != nil {
		t.Fatalf("first rescanRootHint: %v", err)
	}
	if err := second.bootstrapOrCatchUp(context.Background()); err != nil {
		t.Fatalf("second bootstrapOrCatchUp: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home2, "notes", "note.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected propagated delete on second client, got err=%v", err)
	}
}

func TestIntegrationOfflineEditQueuesPendingRescan(t *testing.T) {
	h := newIntegrationHarness(t)

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	rootDir := filepath.Join(home, "notes")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	notePath := filepath.Join(rootDir, "note.txt")
	if err := os.WriteFile(notePath, []byte("online\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(online): %v", err)
	}
	if err := d.AddRoot(context.Background(), rootDir); err != nil {
		t.Fatalf("AddRoot: %v", err)
	}
	root, err := d.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel: %v", err)
	}

	h.Close()
	if err := os.WriteFile(notePath, []byte("offline\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(offline): %v", err)
	}
	if err := d.rescanRootHint(context.Background(), root.RootID, "note.txt"); err != nil {
		t.Fatalf("offline rescan should queue retryable work: %v", err)
	}
	pending, err := d.stateDB.CountPendingOps()
	if err != nil {
		t.Fatalf("CountPendingOps: %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending ops = %d want 1", pending)
	}
	status, err := d.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Connection != protocol.ConnectionDegraded {
		t.Fatalf("connection state = %q want %q", status.Connection, protocol.ConnectionDegraded)
	}
	if status.LastErrorKind != protocol.IssueTransport {
		t.Fatalf("last error kind = %q want %q", status.LastErrorKind, protocol.IssueTransport)
	}
}

func TestIntegrationQueuedOfflineEditFlushesAfterServerRestart(t *testing.T) {
	h := newRestartableHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	rootDir := filepath.Join(home, "notes")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	notePath := filepath.Join(rootDir, "note.txt")
	if err := os.WriteFile(notePath, []byte("online\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(online): %v", err)
	}
	if err := d.AddRoot(context.Background(), rootDir); err != nil {
		t.Fatalf("AddRoot: %v", err)
	}
	root, err := d.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel: %v", err)
	}

	h.Stop()
	if err := os.WriteFile(notePath, []byte("offline\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(offline): %v", err)
	}
	if err := d.rescanRootHint(context.Background(), root.RootID, "note.txt"); err != nil {
		t.Fatalf("offline rescan: %v", err)
	}
	h.Start(t)
	if err := d.flushPendingOps(context.Background()); err != nil {
		t.Fatalf("flushPendingOps after restart: %v", err)
	}
	pending, err := d.stateDB.CountPendingOps()
	if err != nil {
		t.Fatalf("CountPendingOps: %v", err)
	}
	if pending != 0 {
		t.Fatalf("pending ops = %d want 0", pending)
	}
	entries, err := d.stateDB.EntriesForRoot(root.RootID)
	if err != nil {
		t.Fatalf("EntriesForRoot: %v", err)
	}
	if entries["note.txt"].ContentSHA256 == "" {
		t.Fatalf("expected flushed file entry to update local state")
	}
}

func TestIntegrationQueuedFlushConflictCreatesConflictCopy(t *testing.T) {
	h := newRestartableHarness(t)
	defer h.Close()

	home1 := filepath.Join(t.TempDir(), "home-one")
	setHome(t, home1)
	first, cancelFirst := newTestDaemon(t)
	defer cancelFirst()
	firstResp, err := first.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	rootDir1 := filepath.Join(home1, "notes")
	if err := os.MkdirAll(rootDir1, 0o755); err != nil {
		t.Fatalf("MkdirAll(root1): %v", err)
	}
	file1 := filepath.Join(rootDir1, "note.txt")
	if err := os.WriteFile(file1, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(base): %v", err)
	}
	if err := first.AddRoot(context.Background(), rootDir1); err != nil {
		t.Fatalf("first AddRoot: %v", err)
	}

	home2 := filepath.Join(t.TempDir(), "home-two")
	setHome(t, home2)
	second, cancelSecond := newTestDaemon(t)
	defer cancelSecond()
	if _, err := second.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: firstResp.GeneratedRecoveryKey,
	}); err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	root, err := second.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel(second): %v", err)
	}

	h.Stop()
	file2 := filepath.Join(home2, "notes", "note.txt")
	if err := os.WriteFile(file2, []byte("from-second\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(second offline): %v", err)
	}
	if err := second.rescanRootHint(context.Background(), root.RootID, "note.txt"); err != nil {
		t.Fatalf("second offline rescan: %v", err)
	}
	h.Start(t)
	if err := os.WriteFile(file1, []byte("from-first\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(first): %v", err)
	}
	if err := first.rescanRootHint(context.Background(), root.RootID, "note.txt"); err != nil {
		t.Fatalf("first rescan: %v", err)
	}
	if err := second.flushPendingOps(context.Background()); err != nil {
		t.Fatalf("second flushPendingOps: %v", err)
	}

	got, err := os.ReadFile(file2)
	if err != nil {
		t.Fatalf("ReadFile(second note): %v", err)
	}
	if string(got) != "from-first\n" {
		t.Fatalf("original path should keep accepted update, got %q", string(got))
	}
	matches, err := filepath.Glob(filepath.Join(home2, "notes", "note.syna-conflict-*"))
	if err != nil {
		t.Fatalf("Glob(conflicts): %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected one conflict copy, got %v", matches)
	}
	conflict, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("ReadFile(conflict): %v", err)
	}
	if string(conflict) != "from-second\n" {
		t.Fatalf("unexpected conflict contents %q", string(conflict))
	}
	pending, err := second.stateDB.CountPendingOps()
	if err != nil {
		t.Fatalf("CountPendingOps: %v", err)
	}
	if pending != 0 {
		t.Fatalf("pending ops = %d want 0", pending)
	}
}

func TestIntegrationPendingFlushBackoffDoesNotSpin(t *testing.T) {
	h := newIntegrationHarness(t)

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	rootDir := filepath.Join(home, "notes")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	notePath := filepath.Join(rootDir, "note.txt")
	if err := os.WriteFile(notePath, []byte("online\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(online): %v", err)
	}
	if err := d.AddRoot(context.Background(), rootDir); err != nil {
		t.Fatalf("AddRoot: %v", err)
	}
	root, err := d.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel: %v", err)
	}

	h.Close()
	if err := os.WriteFile(notePath, []byte("offline\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(offline): %v", err)
	}
	if err := d.rescanRootHint(context.Background(), root.RootID, "note.txt"); err != nil {
		t.Fatalf("offline rescan: %v", err)
	}
	if err := d.flushPendingOps(context.Background()); err == nil {
		t.Fatalf("expected first flush to fail while server is unavailable")
	}
	ops, err := d.stateDB.ListPendingOps()
	if err != nil {
		t.Fatalf("ListPendingOps: %v", err)
	}
	if len(ops) != 1 || ops[0].RetryCount != 1 || ops[0].NextRetryAt.IsZero() {
		t.Fatalf("expected one backed-off pending op, got %+v", ops)
	}
	if err := d.flushPendingOps(context.Background()); err != nil {
		t.Fatalf("second immediate flush should skip backed-off op: %v", err)
	}
	ops, err = d.stateDB.ListPendingOps()
	if err != nil {
		t.Fatalf("ListPendingOps(after): %v", err)
	}
	if len(ops) != 1 || ops[0].RetryCount != 1 {
		t.Fatalf("retry count changed during skipped flush: %+v", ops)
	}
}

func TestIntegrationRootRemoveAndReAdd(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home1 := filepath.Join(t.TempDir(), "home-one")
	setHome(t, home1)
	first, cancelFirst := newTestDaemon(t)
	defer cancelFirst()
	firstResp, err := first.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}

	rootDir1 := filepath.Join(home1, "notes")
	if err := os.MkdirAll(rootDir1, 0o755); err != nil {
		t.Fatalf("MkdirAll(root1): %v", err)
	}
	file1 := filepath.Join(rootDir1, "note.txt")
	if err := os.WriteFile(file1, []byte("one\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(one): %v", err)
	}
	if err := first.AddRoot(context.Background(), rootDir1); err != nil {
		t.Fatalf("first AddRoot: %v", err)
	}
	if err := first.RemoveRoot(context.Background(), rootDir1); err != nil {
		t.Fatalf("first RemoveRoot: %v", err)
	}
	if err := os.WriteFile(file1, []byte("two\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(two): %v", err)
	}
	if err := first.AddRoot(context.Background(), rootDir1); err != nil {
		t.Fatalf("second AddRoot: %v", err)
	}

	home2 := filepath.Join(t.TempDir(), "home-two")
	setHome(t, home2)
	second, cancelSecond := newTestDaemon(t)
	defer cancelSecond()
	if _, err := second.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: firstResp.GeneratedRecoveryKey,
	}); err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	if err := second.bootstrap(context.Background()); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(home2, "notes", "note.txt"))
	if err != nil {
		t.Fatalf("ReadFile(second): %v", err)
	}
	if string(got) != "two\n" {
		t.Fatalf("unexpected re-added contents %q", string(got))
	}
}
