package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"syna/internal/client/scanner"
	"syna/internal/client/state"
	commoncfg "syna/internal/common/config"
	"syna/internal/common/protocol"
	"syna/internal/server/admin"
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
	return newIntegrationHarnessWithMaxEventFetchPage(t, 1000)
}

func newIntegrationHarnessWithMaxEventFetchPage(t *testing.T, maxEventFetchPage int) *integrationHarness {
	return newIntegrationHarnessWithObserver(t, maxEventFetchPage, nil)
}

func newIntegrationHarnessWithObserver(t *testing.T, maxEventFetchPage int, observe func(string)) *integrationHarness {
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
		MaxEventFetchPage: maxEventFetchPage,
		MaxPlainChunkSize: 4 << 20,
		MaxEventBodyBytes: 1 << 20,
		MaxSnapshotBody:   16 << 20,
		MaxSnapshotPlain:  16 << 20,
		MaxWSClients:      8,
	}
	handler := api.New(cfg, database, objectstore.New(dataDir), hub.New(cfg.MaxWSClients, log.New(io.Discard, "", 0)), log.New(io.Discard, "", 0)).Handler()
	if observe != nil {
		next := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			observe(r.URL.Path)
			next.ServeHTTP(w, r)
		})
	}
	server := httptest.NewServer(handler)
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

func TestIntegrationPurgedWorkspaceStopsAndCanBeExplicitlyRecreated(t *testing.T) {
	h := newRestartableHarness(t)
	defer h.Close()
	d, cancel := newTestDaemon(t)
	defer cancel()

	created, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	key := created.GeneratedRecoveryKey
	workspaceID := created.WorkspaceID
	if key == "" || workspaceID == "" {
		t.Fatalf("missing created workspace credentials: %+v", created)
	}

	h.Stop()
	if err := admin.PurgeWorkspace(h.serverDB, objectstore.New(h.dataDir), h.dataDir, workspaceID, io.Discard); err != nil {
		t.Fatalf("PurgeWorkspace: %v", err)
	}
	h.Start(t)

	reconnectCtx, stopReconnect := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.reconnectLoop(reconnectCtx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, statusErr := d.Status()
		if statusErr != nil {
			t.Fatalf("Status: %v", statusErr)
		}
		if status.Connection == protocol.ConnectionWorkspacePurged {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("client did not enter purged state: %+v", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopReconnect()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconnect loop did not stop")
	}
	keyring, err := d.configs.LoadKeyring()
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if keyring.WorkspaceKey != key {
		t.Fatalf("retained key = %q want %q", keyring.WorkspaceKey, key)
	}

	activeCtx, stopActive := context.WithCancel(context.Background())
	defer stopActive()
	d.runCtx = activeCtx
	recreated, err := d.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: key,
		Recreate:    true,
	})
	if err != nil {
		t.Fatalf("recreate Connect: %v", err)
	}
	if recreated.WorkspaceID != workspaceID || recreated.GeneratedRecoveryKey != "" {
		t.Fatalf("unexpected recreate response: %+v", recreated)
	}
	if seq, err := h.serverDB.CurrentSeq(workspaceID); err != nil || seq != 0 {
		t.Fatalf("recreated workspace seq = %d err=%v", seq, err)
	}
	d.mu.Lock()
	reconnectStarted := d.reconnectCancel != nil
	d.mu.Unlock()
	if !reconnectStarted {
		t.Fatal("explicit recreation did not start a replacement reconnect loop")
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

func TestIntegrationConnectDoesNotReportLiveBeforeWebsocket(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	status, err := d.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Connection == protocol.ConnectionLive {
		t.Fatal("connect reported live before a WebSocket subscription was established")
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
	if _, err := d.stateDB.SQL.Exec(`UPDATE workspace_state SET last_server_seq = 0 WHERE singleton = 1`); err != nil {
		t.Fatalf("reset last_server_seq: %v", err)
	}
	if err := d.consumeRemoteEvent(context.Background(), *rootAdd); err != nil {
		t.Fatalf("apply own root_add: %v", err)
	}
	st, err := d.stateDB.LoadWorkspaceState()
	if err != nil {
		t.Fatalf("LoadWorkspaceState: %v", err)
	}
	if st.LastServerSeq < rootAdd.Seq {
		t.Fatalf("root_add did not advance cursor: got %d want at least %d", st.LastServerSeq, rootAdd.Seq)
	}
	root, err := d.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel: %v", err)
	}
	if root.State != protocol.RootStateActive {
		t.Fatalf("own root_add replay changed root state to %q", root.State)
	}
}

func TestIntegrationAddRootRecoversPeerDirectoryHeadRace(t *testing.T) {
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

	rootDir1 := filepath.Join(home1, "notes")
	if err := os.MkdirAll(filepath.Join(rootDir1, "deep", "empty"), 0o755); err != nil {
		t.Fatalf("MkdirAll(root1): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir1, "deep", "note.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(root1): %v", err)
	}
	setHome(t, home1)
	peerSubmittedDirs := false
	err = first.AddRootWithProgress(context.Background(), rootDir1, func(progress AddProgress) {
		if peerSubmittedDirs || progress.Stage != "syncing" || progress.Message != "synced file" {
			return
		}
		peerSubmittedDirs = true
		setHome(t, home2)
		if err := second.bootstrapOrCatchUp(context.Background()); err != nil {
			t.Fatalf("second bootstrapOrCatchUp during add: %v", err)
		}
		root, err := second.stateDB.RootByHomeRel("notes")
		if err != nil {
			t.Fatalf("second RootByHomeRel during add: %v", err)
		}
		if err := second.rescanRoot(context.Background(), root.RootID); err != nil {
			t.Fatalf("second rescanRoot during add: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("first AddRootWithProgress: %v", err)
	}
	if !peerSubmittedDirs {
		t.Fatal("test did not submit peer directory heads during initial upload")
	}
	if err := second.bootstrapOrCatchUp(context.Background()); err != nil {
		t.Fatalf("second final bootstrapOrCatchUp: %v", err)
	}

	for _, rootDir := range []string{rootDir1, filepath.Join(home2, "notes")} {
		got, err := os.ReadFile(filepath.Join(rootDir, "deep", "note.txt"))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", rootDir, err)
		}
		if string(got) != "original\n" {
			t.Fatalf("contents at %s = %q", rootDir, string(got))
		}
		if info, err := os.Stat(filepath.Join(rootDir, "deep", "empty")); err != nil || !info.IsDir() {
			t.Fatalf("empty directory at %s: info=%v err=%v", rootDir, info, err)
		}
	}
	for name, daemon := range map[string]*Daemon{"first": first, "second": second} {
		pending, err := daemon.stateDB.CountPendingOps()
		if err != nil {
			t.Fatalf("%s CountPendingOps: %v", name, err)
		}
		if pending != 0 {
			t.Fatalf("%s pending ops = %d want 0", name, pending)
		}
	}
	bootstrap, err := first.conn.Bootstrap(context.Background())
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(bootstrap.Roots) != 1 {
		t.Fatalf("bootstrap roots = %d want 1", len(bootstrap.Roots))
	}
	if bootstrap.Roots[0].LatestSnapshotObjectID != "" || bootstrap.Roots[0].LatestSnapshotSeq != 0 {
		t.Fatalf("collision published a partial snapshot: %+v", bootstrap.Roots[0])
	}

	for {
		select {
		case <-first.changeCh:
		default:
			goto drained
		}
	}
drained:
	if err := os.WriteFile(filepath.Join(rootDir1, "after-add.txt"), []byte("watched\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(after add): %v", err)
	}
	select {
	case <-first.changeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("successfully recovered root was not watched immediately")
	}
}

func TestIntegrationAddRootPreservesInitialFileConflict(t *testing.T) {
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

	rootDir1 := filepath.Join(home1, "notes")
	if err := os.MkdirAll(rootDir1, 0o755); err != nil {
		t.Fatalf("MkdirAll(root1): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir1, "note.txt"), []byte("from-first\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(first): %v", err)
	}
	setHome(t, home1)
	peerSubmittedFile := false
	err = first.AddRootWithProgress(context.Background(), rootDir1, func(progress AddProgress) {
		if peerSubmittedFile || progress.Stage != "syncing" || progress.Message != "uploading file" {
			return
		}
		peerSubmittedFile = true
		setHome(t, home2)
		if err := second.bootstrapOrCatchUp(context.Background()); err != nil {
			t.Fatalf("second bootstrapOrCatchUp during add: %v", err)
		}
		root, err := second.stateDB.RootByHomeRel("notes")
		if err != nil {
			t.Fatalf("second RootByHomeRel during add: %v", err)
		}
		file2 := filepath.Join(home2, "notes", "note.txt")
		if err := os.WriteFile(file2, []byte("from-second\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(second): %v", err)
		}
		if err := second.rescanRootHint(context.Background(), root.RootID, "note.txt"); err != nil {
			t.Fatalf("second rescanRootHint during add: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("first AddRootWithProgress: %v", err)
	}
	if !peerSubmittedFile {
		t.Fatal("test did not submit peer file during initial upload")
	}
	if err := second.bootstrapOrCatchUp(context.Background()); err != nil {
		t.Fatalf("second final bootstrapOrCatchUp: %v", err)
	}

	for _, rootDir := range []string{rootDir1, filepath.Join(home2, "notes")} {
		got, err := os.ReadFile(filepath.Join(rootDir, "note.txt"))
		if err != nil {
			t.Fatalf("ReadFile(remote winner at %s): %v", rootDir, err)
		}
		if string(got) != "from-second\n" {
			t.Fatalf("remote winner at %s = %q", rootDir, string(got))
		}
		matches, err := filepath.Glob(filepath.Join(rootDir, "note.syna-conflict-*"))
		if err != nil {
			t.Fatalf("Glob(%s): %v", rootDir, err)
		}
		if len(matches) != 1 {
			t.Fatalf("conflict copies at %s = %v", rootDir, matches)
		}
		conflictBytes, err := os.ReadFile(matches[0])
		if err != nil {
			t.Fatalf("ReadFile(conflict at %s): %v", rootDir, err)
		}
		if string(conflictBytes) != "from-first\n" {
			t.Fatalf("conflict contents at %s = %q", rootDir, string(conflictBytes))
		}
	}
	for name, daemon := range map[string]*Daemon{"first": first, "second": second} {
		pending, err := daemon.stateDB.CountPendingOps()
		if err != nil {
			t.Fatalf("%s CountPendingOps: %v", name, err)
		}
		if pending != 0 {
			t.Fatalf("%s pending ops = %d want 0", name, pending)
		}
	}
}

func TestIntegrationAddRootSkipsSnapshotAfterInterleavedPeerEdit(t *testing.T) {
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

	rootDir1 := filepath.Join(home1, "notes")
	if err := os.MkdirAll(rootDir1, 0o755); err != nil {
		t.Fatalf("MkdirAll(root1): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir1, "note.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(initial): %v", err)
	}
	setHome(t, home1)
	peerEdited := false
	err = first.AddRootWithProgress(context.Background(), rootDir1, func(progress AddProgress) {
		if peerEdited || progress.Stage != "syncing" || progress.Message != "synced file" {
			return
		}
		peerEdited = true
		setHome(t, home2)
		if err := second.bootstrapOrCatchUp(context.Background()); err != nil {
			t.Fatalf("second bootstrapOrCatchUp during add: %v", err)
		}
		root, err := second.stateDB.RootByHomeRel("notes")
		if err != nil {
			t.Fatalf("second RootByHomeRel during add: %v", err)
		}
		if err := os.WriteFile(filepath.Join(home2, "notes", "note.txt"), []byte("peer-newer\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(peer edit): %v", err)
		}
		if err := second.rescanRootHint(context.Background(), root.RootID, "note.txt"); err != nil {
			t.Fatalf("second rescanRootHint during add: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("first AddRootWithProgress: %v", err)
	}
	if !peerEdited {
		t.Fatal("test did not submit the interleaved peer edit")
	}
	bootstrap, err := first.conn.Bootstrap(context.Background())
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(bootstrap.Roots) != 1 {
		t.Fatalf("bootstrap roots = %d want 1", len(bootstrap.Roots))
	}
	if bootstrap.Roots[0].LatestSnapshotObjectID != "" || bootstrap.Roots[0].LatestSnapshotSeq != 0 {
		t.Fatalf("interleaved peer edit produced a stale snapshot: %+v", bootstrap.Roots[0])
	}

	home3 := filepath.Join(t.TempDir(), "home-three")
	setHome(t, home3)
	third, cancelThird := newTestDaemon(t)
	defer cancelThird()
	if _, err := third.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: firstResp.GeneratedRecoveryKey,
	}); err != nil {
		t.Fatalf("third Connect: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(home3, "notes", "note.txt"))
	if err != nil {
		t.Fatalf("third ReadFile(note): %v", err)
	}
	if string(got) != "peer-newer\n" {
		t.Fatalf("third note contents = %q", string(got))
	}
}

func TestIntegrationAddRootPreservesLaterDirectoryTypeConflict(t *testing.T) {
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

	rootDir1 := filepath.Join(home1, "notes")
	if err := os.MkdirAll(filepath.Join(rootDir1, "node"), 0o755); err != nil {
		t.Fatalf("MkdirAll(root1): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir1, "node", "child.txt"), []byte("local-child\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(child): %v", err)
	}
	setHome(t, home1)
	peerReplacedDirectory := false
	err = first.AddRootWithProgress(context.Background(), rootDir1, func(progress AddProgress) {
		if peerReplacedDirectory || progress.Stage != "syncing" || progress.Message != "synced file" {
			return
		}
		peerReplacedDirectory = true
		setHome(t, home2)
		if err := second.bootstrapOrCatchUp(context.Background()); err != nil {
			t.Fatalf("second bootstrapOrCatchUp during add: %v", err)
		}
		root, err := second.stateDB.RootByHomeRel("notes")
		if err != nil {
			t.Fatalf("second RootByHomeRel during add: %v", err)
		}
		node2 := filepath.Join(home2, "notes", "node")
		if err := os.RemoveAll(node2); err != nil {
			t.Fatalf("RemoveAll(peer node): %v", err)
		}
		if err := os.WriteFile(node2, []byte("peer-file\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(peer node): %v", err)
		}
		if err := second.rescanRoot(context.Background(), root.RootID); err != nil {
			t.Fatalf("second rescanRoot during add: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("first AddRootWithProgress: %v", err)
	}
	if !peerReplacedDirectory {
		t.Fatal("test did not replace the initial directory from the peer")
	}
	if err := second.bootstrapOrCatchUp(context.Background()); err != nil {
		t.Fatalf("second final bootstrapOrCatchUp: %v", err)
	}

	for _, rootDir := range []string{rootDir1, filepath.Join(home2, "notes")} {
		got, err := os.ReadFile(filepath.Join(rootDir, "node"))
		if err != nil {
			t.Fatalf("ReadFile(peer node at %s): %v", rootDir, err)
		}
		if string(got) != "peer-file\n" {
			t.Fatalf("peer node at %s = %q", rootDir, string(got))
		}
		matches, err := filepath.Glob(filepath.Join(rootDir, "node.syna-conflict-*"))
		if err != nil {
			t.Fatalf("Glob(%s): %v", rootDir, err)
		}
		if len(matches) != 1 {
			t.Fatalf("directory conflict copies at %s = %v", rootDir, matches)
		}
		child, err := os.ReadFile(filepath.Join(matches[0], "child.txt"))
		if err != nil {
			t.Fatalf("ReadFile(preserved child at %s): %v", rootDir, err)
		}
		if string(child) != "local-child\n" {
			t.Fatalf("preserved child at %s = %q", rootDir, string(child))
		}
	}
}

func TestIntegrationAddRootQueuesRecoveryAfterInitialTransportFailure(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	eventSubmits := 0
	d.conn.HTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost && req.URL.Path == "/v1/events" {
			eventSubmits++
			if eventSubmits == 2 {
				return nil, errors.New("initial upload transport failed")
			}
		}
		return http.DefaultTransport.RoundTrip(req)
	})

	rootDir := filepath.Join(home, "notes")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "note.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(root): %v", err)
	}
	err := d.AddRoot(context.Background(), rootDir)
	if err == nil || !strings.Contains(err.Error(), "initial root upload") {
		t.Fatalf("AddRoot error = %v, want actionable initial-upload error", err)
	}
	root, err := d.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel: %v", err)
	}
	if root.State != protocol.RootStateActive {
		t.Fatalf("root state = %s want %s", root.State, protocol.RootStateActive)
	}
	pending, err := d.stateDB.CountPendingOps()
	if err != nil {
		t.Fatalf("CountPendingOps: %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending ops = %d want 1", pending)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "after-failure.txt"), []byte("watched\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(after failure): %v", err)
	}
	select {
	case <-d.changeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("active root had queued recovery but no watcher after transport failure")
	}
}

func TestIntegrationAddRootQueuesRecoveryAfterLostRootAddResponse(t *testing.T) {
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
	dropped := false
	d.conn.HTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(req)
		if err == nil && !dropped && req.Method == http.MethodPost && req.URL.Path == "/v1/events" {
			dropped = true
			_ = response.Body.Close()
			return nil, errors.New("root_add response lost")
		}
		return response, err
	})

	rootDir := filepath.Join(home, "notes")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "note.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(root): %v", err)
	}
	err = d.AddRoot(context.Background(), rootDir)
	if err == nil || !strings.Contains(err.Error(), "root_add response lost") {
		t.Fatalf("AddRoot error = %v, want lost-response error", err)
	}
	if !dropped {
		t.Fatal("test did not drop the committed root_add response")
	}
	ops, err := d.stateDB.ListPendingOps()
	if err != nil {
		t.Fatalf("ListPendingOps: %v", err)
	}
	if len(ops) != 1 || ops[0].OpType != "recover_initial_root" || ops[0].BaseSeq != 0 {
		t.Fatalf("pending ops = %+v, want unknown-incarnation initial recovery", ops)
	}
	if err := d.flushPendingOps(context.Background()); err != nil {
		t.Fatalf("flushPendingOps: %v", err)
	}
	pending, err := d.stateDB.CountPendingOps()
	if err != nil {
		t.Fatalf("CountPendingOps: %v", err)
	}
	if pending != 0 {
		t.Fatalf("pending ops after recovery = %d want 0", pending)
	}

	home2 := filepath.Join(t.TempDir(), "home-two")
	setHome(t, home2)
	second, cancelSecond := newTestDaemon(t)
	defer cancelSecond()
	if _, err := second.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: resp.GeneratedRecoveryKey,
	}); err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(home2, "notes", "note.txt"))
	if err != nil {
		t.Fatalf("second ReadFile(note): %v", err)
	}
	if string(got) != "local\n" {
		t.Fatalf("second note contents = %q", string(got))
	}
}

func TestIntegrationAddRootRejectsServerWithoutIncarnationCapabilityBeforeMutation(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	d.conn.HTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet && req.URL.Path == "/healthz" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
				Request:    req,
			}, nil
		}
		return http.DefaultTransport.RoundTrip(req)
	})
	rootDir := filepath.Join(home, "notes")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	err := d.AddRoot(context.Background(), rootDir)
	if err == nil || !strings.Contains(err.Error(), "server upgrade required") {
		t.Fatalf("AddRoot error = %v, want preflight upgrade error", err)
	}
	current, err := h.serverDB.CurrentSeq(d.cfg.WorkspaceID)
	if err != nil {
		t.Fatalf("CurrentSeq: %v", err)
	}
	if current != 0 {
		t.Fatalf("server sequence = %d want 0 after capability rejection", current)
	}
	roots, err := d.stateDB.ListRoots()
	if err != nil {
		t.Fatalf("ListRoots: %v", err)
	}
	if len(roots) != 0 {
		t.Fatalf("local roots = %+v, want none after capability rejection", roots)
	}
}

func TestIntegrationAddRootReportsPostConflictReconcileFailure(t *testing.T) {
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

	rootDir1 := filepath.Join(home1, "notes")
	if err := os.MkdirAll(filepath.Join(rootDir1, "deep"), 0o755); err != nil {
		t.Fatalf("MkdirAll(root1): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir1, "deep", "note.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(root1): %v", err)
	}
	setHome(t, home1)
	peerSubmittedDirs := false
	postHookEventSubmits := 0
	err = first.AddRootWithProgress(context.Background(), rootDir1, func(progress AddProgress) {
		if peerSubmittedDirs || progress.Stage != "syncing" || progress.Message != "synced file" {
			return
		}
		peerSubmittedDirs = true
		setHome(t, home2)
		if err := second.bootstrapOrCatchUp(context.Background()); err != nil {
			t.Fatalf("second bootstrapOrCatchUp during add: %v", err)
		}
		root, err := second.stateDB.RootByHomeRel("notes")
		if err != nil {
			t.Fatalf("second RootByHomeRel during add: %v", err)
		}
		if err := second.rescanRoot(context.Background(), root.RootID); err != nil {
			t.Fatalf("second rescanRoot during add: %v", err)
		}
		first.conn.HTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodPost && req.URL.Path == "/v1/events" {
				postHookEventSubmits++
				if postHookEventSubmits == 2 {
					return nil, errors.New("post-conflict reconcile transport failed")
				}
			}
			return http.DefaultTransport.RoundTrip(req)
		})
	})
	if err == nil || !strings.Contains(err.Error(), "reconcile initial path conflict") {
		t.Fatalf("first AddRootWithProgress error = %v, want reconciliation error", err)
	}
	if postHookEventSubmits < 2 {
		t.Fatalf("post-hook event submits = %d want at least 2", postHookEventSubmits)
	}
	pending, err := first.stateDB.CountPendingOps()
	if err != nil {
		t.Fatalf("CountPendingOps: %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending ops = %d want 1", pending)
	}
	first.conn.HTTPClient.Transport = http.DefaultTransport
	if err := first.flushPendingOps(context.Background()); err != nil {
		t.Fatalf("flushPendingOps: %v", err)
	}
	pending, err = first.stateDB.CountPendingOps()
	if err != nil {
		t.Fatalf("CountPendingOps(after): %v", err)
	}
	if pending != 0 {
		t.Fatalf("pending ops after recovery = %d want 0", pending)
	}
}

func TestIntegrationInitialUploadDoesNotCrossReplacementIncarnation(t *testing.T) {
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

	rootDir1 := filepath.Join(home1, "notes")
	if err := os.MkdirAll(rootDir1, 0o755); err != nil {
		t.Fatalf("MkdirAll(root1): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir1, "old-only.txt"), []byte("old incarnation\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(old-only): %v", err)
	}
	setHome(t, home1)
	eventSubmits := 0
	replaced := false
	var replacementHead int64
	first.conn.HTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost && req.URL.Path == "/v1/events" {
			eventSubmits++
			if eventSubmits == 2 {
				setHome(t, home2)
				if err := second.bootstrapOrCatchUp(context.Background()); err != nil {
					t.Fatalf("second bootstrapOrCatchUp: %v", err)
				}
				rootDir2 := filepath.Join(home2, "notes")
				if err := second.RemoveRoot(context.Background(), rootDir2); err != nil {
					t.Fatalf("second RemoveRoot: %v", err)
				}
				if err := os.RemoveAll(rootDir2); err != nil {
					t.Fatalf("RemoveAll(second old incarnation): %v", err)
				}
				if err := os.MkdirAll(rootDir2, 0o755); err != nil {
					t.Fatalf("MkdirAll(second replacement): %v", err)
				}
				if err := second.AddRoot(context.Background(), rootDir2); err != nil {
					t.Fatalf("second replacement AddRoot: %v", err)
				}
				replaced = true
				replacementHead, err = h.serverDB.CurrentSeq(first.cfg.WorkspaceID)
				if err != nil {
					t.Fatalf("CurrentSeq(replacement): %v", err)
				}
				return http.DefaultTransport.RoundTrip(req)
			}
		}
		return http.DefaultTransport.RoundTrip(req)
	})
	err = first.AddRoot(context.Background(), rootDir1)
	if err == nil || !strings.Contains(err.Error(), "root_incarnation_mismatch") {
		t.Fatalf("first AddRoot error = %v, want incarnation-mismatch error", err)
	}
	if !replaced {
		t.Fatal("test did not replace the root incarnation")
	}
	root, err := first.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("first RootByHomeRel: %v", err)
	}
	if !first.isRootStaged(root.RootID) {
		t.Fatal("failed initial add was not staged after remote replacement")
	}
	ops, err := first.stateDB.ListPendingOps()
	if err != nil {
		t.Fatalf("ListPendingOps: %v", err)
	}
	if len(ops) != 1 || ops[0].OpType != "recover_initial_root" || ops[0].BaseSeq == 0 {
		t.Fatalf("pending ops = %+v, want incarnation-bound recovery", ops)
	}
	beforeFlush, err := h.serverDB.CurrentSeq(first.cfg.WorkspaceID)
	if err != nil {
		t.Fatalf("CurrentSeq(before): %v", err)
	}
	if beforeFlush != replacementHead {
		t.Fatalf("replacement head advanced from %d to %d after rejected initial event", replacementHead, beforeFlush)
	}
	if err := os.RemoveAll(rootDir1); err != nil {
		t.Fatalf("RemoveAll(first missing root): %v", err)
	}
	err = first.rescanRootHintWithRetry(context.Background(), root.RootID, "", true, false, true, ops[0].BaseSeq)
	var incarnationConflict *RootIncarnationConflictError
	if !errors.As(err, &incarnationConflict) {
		t.Fatalf("forced missing-root rescan error = %v, want incarnation conflict", err)
	}
	afterRejectedRemove, err := h.serverDB.CurrentSeq(first.cfg.WorkspaceID)
	if err != nil {
		t.Fatalf("CurrentSeq(after rejected remove): %v", err)
	}
	if afterRejectedRemove != beforeFlush {
		t.Fatalf("replacement head advanced from %d to %d after rejected old root_remove", beforeFlush, afterRejectedRemove)
	}
	if err := first.flushPendingOps(context.Background()); err != nil {
		t.Fatalf("flushPendingOps: %v", err)
	}
	afterFlush, err := h.serverDB.CurrentSeq(first.cfg.WorkspaceID)
	if err != nil {
		t.Fatalf("CurrentSeq(after): %v", err)
	}
	if afterFlush != beforeFlush {
		t.Fatalf("replacement head advanced from %d to %d while flushing old recovery", beforeFlush, afterFlush)
	}
	pending, err := first.stateDB.CountPendingOps()
	if err != nil {
		t.Fatalf("CountPendingOps: %v", err)
	}
	if pending != 0 {
		t.Fatalf("pending ops after rejecting old recovery = %d want 0", pending)
	}
}

func TestIntegrationAddRootStopsFinalizingAfterConcurrentRemove(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	first, cancelFirst := newTestDaemon(t)
	defer cancelFirst()
	firstResp, err := first.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	second, cancelSecond := newTestDaemon(t)
	defer cancelSecond()
	if _, err := second.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: firstResp.GeneratedRecoveryKey,
	}); err != nil {
		t.Fatalf("second Connect: %v", err)
	}

	rootDir := filepath.Join(home, "notes")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "note.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(root): %v", err)
	}
	removed := false
	err = first.AddRootWithProgress(context.Background(), rootDir, func(progress AddProgress) {
		if removed || progress.Stage != "syncing" || progress.DoneEntries != progress.TotalEntries {
			return
		}
		removed = true
		if err := second.bootstrapOrCatchUp(context.Background()); err != nil {
			t.Fatalf("second bootstrapOrCatchUp: %v", err)
		}
		if err := second.RemoveRoot(context.Background(), rootDir); err != nil {
			t.Fatalf("second RemoveRoot: %v", err)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "no longer active after initial sync") {
		t.Fatalf("AddRootWithProgress error = %v, want concurrent removal error", err)
	}
	if !removed {
		t.Fatal("test did not submit the concurrent root removal")
	}
	root, err := first.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel: %v", err)
	}
	if root.State != protocol.RootStateRemoved {
		t.Fatalf("root state = %s want %s", root.State, protocol.RootStateRemoved)
	}

	if err := os.WriteFile(filepath.Join(rootDir, "after-remove.txt"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(after remove): %v", err)
	}
	select {
	case change := <-first.changeCh:
		t.Fatalf("removed root retained watcher and emitted change: %+v", change)
	case <-time.After(750 * time.Millisecond):
	}
}

func TestIntegrationAddRootReconcilesRemoveDuringSnapshotPublication(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	first, cancelFirst := newTestDaemon(t)
	defer cancelFirst()
	firstResp, err := first.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	second, cancelSecond := newTestDaemon(t)
	defer cancelSecond()
	if _, err := second.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: firstResp.GeneratedRecoveryKey,
	}); err != nil {
		t.Fatalf("second Connect: %v", err)
	}

	rootDir := filepath.Join(home, "notes")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "note.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(root): %v", err)
	}
	removed := false
	err = first.AddRootWithProgress(context.Background(), rootDir, func(progress AddProgress) {
		if removed || progress.Stage != "finalizing" {
			return
		}
		removed = true
		if err := second.bootstrapOrCatchUp(context.Background()); err != nil {
			t.Fatalf("second bootstrapOrCatchUp: %v", err)
		}
		if err := second.RemoveRoot(context.Background(), rootDir); err != nil {
			t.Fatalf("second RemoveRoot: %v", err)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "no longer active after initial sync") {
		t.Fatalf("AddRootWithProgress error = %v, want concurrent removal error", err)
	}
	if !removed {
		t.Fatal("test did not submit the concurrent root removal")
	}
	root, err := first.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel: %v", err)
	}
	if root.State != protocol.RootStateRemoved {
		t.Fatalf("root state = %s want %s", root.State, protocol.RootStateRemoved)
	}

	if err := os.WriteFile(filepath.Join(rootDir, "after-remove.txt"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(after remove): %v", err)
	}
	select {
	case change := <-first.changeCh:
		t.Fatalf("removed root retained watcher and emitted change: %+v", change)
	case <-time.After(750 * time.Millisecond):
	}
}

func TestIntegrationAddRootStagesConcurrentReplacement(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	first, cancelFirst := newTestDaemon(t)
	defer cancelFirst()
	firstResp, err := first.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	second, cancelSecond := newTestDaemon(t)
	defer cancelSecond()
	if _, err := second.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: firstResp.GeneratedRecoveryKey,
	}); err != nil {
		t.Fatalf("second Connect: %v", err)
	}

	rootDir := filepath.Join(home, "notes")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "note.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(root): %v", err)
	}
	replaced := false
	err = first.AddRootWithProgress(context.Background(), rootDir, func(progress AddProgress) {
		if replaced || progress.Stage != "finalizing" {
			return
		}
		replaced = true
		if err := second.bootstrapOrCatchUp(context.Background()); err != nil {
			t.Fatalf("second bootstrapOrCatchUp: %v", err)
		}
		if err := second.RemoveRoot(context.Background(), rootDir); err != nil {
			t.Fatalf("second RemoveRoot: %v", err)
		}
		if err := second.AddRoot(context.Background(), rootDir); err != nil {
			t.Fatalf("second AddRoot replacement: %v", err)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "replaced during initial sync") {
		t.Fatalf("first AddRootWithProgress error = %v, want replacement error", err)
	}
	if !replaced {
		t.Fatal("test did not replace the root")
	}
	root, rootErr := first.stateDB.RootByHomeRel("notes")
	if rootErr != nil {
		t.Fatalf("RootByHomeRel: %v", rootErr)
	}
	if !first.isRootStaged(root.RootID) {
		t.Fatal("replacement root was not staged before reconciliation")
	}
}

func TestIntegrationAddRootReconcilesRemoveAfterSnapshotPublication(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	first, cancelFirst := newTestDaemon(t)
	defer cancelFirst()
	firstResp, err := first.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	second, cancelSecond := newTestDaemon(t)
	defer cancelSecond()
	if _, err := second.Connect(context.Background(), ConnectRequest{
		ServerURL:   h.serverURL,
		RecoveryKey: firstResp.GeneratedRecoveryKey,
	}); err != nil {
		t.Fatalf("second Connect: %v", err)
	}

	rootDir := filepath.Join(home, "notes")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "note.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(root): %v", err)
	}
	removed := false
	first.conn.HTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil || removed || req.Method != http.MethodPost || req.URL.Path != "/v1/snapshots" || resp.StatusCode != http.StatusOK {
			return resp, err
		}
		removed = true
		if err := second.bootstrapOrCatchUp(context.Background()); err != nil {
			t.Fatalf("second bootstrapOrCatchUp: %v", err)
		}
		if err := second.RemoveRoot(context.Background(), rootDir); err != nil {
			t.Fatalf("second RemoveRoot: %v", err)
		}
		return resp, nil
	})

	err = first.AddRoot(context.Background(), rootDir)
	if err == nil || !strings.Contains(err.Error(), "no longer active after initial sync") {
		t.Fatalf("AddRoot error = %v, want concurrent removal error", err)
	}
	if !removed {
		t.Fatal("test did not submit the post-publication root removal")
	}
	root, err := first.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel: %v", err)
	}
	if root.State != protocol.RootStateRemoved {
		t.Fatalf("root state = %s want %s", root.State, protocol.RootStateRemoved)
	}
}

func TestIntegrationAddRootKeepsWatcherWhenSnapshotPublicationFails(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	d.conn.HTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost && req.URL.Path == "/v1/snapshots" {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Status:     "503 Service Unavailable",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"code":"temporarily_unavailable","message":"snapshot unavailable"}`)),
				Request:    req,
			}, nil
		}
		return http.DefaultTransport.RoundTrip(req)
	})

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

	changedPath := filepath.Join(rootDir, "after-add.txt")
	if err := os.WriteFile(changedPath, []byte("watched\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(after add): %v", err)
	}
	select {
	case change := <-d.changeCh:
		if change.RootID == "" {
			t.Fatalf("watcher emitted change without root: %+v", change)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active root was not watched after snapshot publication failure")
	}
}

func TestIntegrationAddRootKeepsWatcherWhenVerificationFails(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	failed := false
	d.conn.HTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if !failed && req.Method == http.MethodGet && req.URL.Path == "/v1/bootstrap" {
			failed = true
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Status:     "503 Service Unavailable",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"code":"temporarily_unavailable","message":"bootstrap unavailable"}`)),
				Request:    req,
			}, nil
		}
		return http.DefaultTransport.RoundTrip(req)
	})

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
	if !failed {
		t.Fatal("test did not fail root incarnation verification")
	}

	if err := os.WriteFile(filepath.Join(rootDir, "after-add.txt"), []byte("watched\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(after add): %v", err)
	}
	select {
	case <-d.changeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("active root was not watched after transient verification failure")
	}
}

func TestIntegrationAddRootKeepsWatcherWhenFinalCatchUpFails(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	eventFetches := 0
	d.conn.HTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet && req.URL.Path == "/v1/events" {
			eventFetches++
			if eventFetches == 2 {
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Status:     "503 Service Unavailable",
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"code":"temporarily_unavailable","message":"events unavailable"}`)),
					Request:    req,
				}, nil
			}
		}
		return http.DefaultTransport.RoundTrip(req)
	})

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
	if eventFetches < 2 {
		t.Fatalf("event fetch count = %d want at least 2", eventFetches)
	}

	if err := os.WriteFile(filepath.Join(rootDir, "after-add.txt"), []byte("watched\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(after add): %v", err)
	}
	select {
	case <-d.changeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("active root was not watched after transient final catch-up failure")
	}
}

func TestIntegrationAddRootRetriesPersistentFinalizationFailure(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	retryCtx, retryCancel := context.WithCancel(context.Background())
	defer retryCancel()
	d.runCtx = retryCtx
	eventFetches := 0
	d.conn.HTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet && req.URL.Path == "/v1/events" {
			eventFetches++
			if eventFetches == 2 || eventFetches == 3 {
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Status:     "503 Service Unavailable",
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"code":"temporarily_unavailable","message":"events unavailable"}`)),
					Request:    req,
				}, nil
			}
		}
		return http.DefaultTransport.RoundTrip(req)
	})

	rootDir := filepath.Join(home, "notes")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "note.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(root): %v", err)
	}
	if err := d.AddRoot(context.Background(), rootDir); err == nil || !strings.Contains(err.Error(), "catch up after initial sync") {
		t.Fatalf("AddRoot error = %v, want finalization catch-up error", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		changedPath := filepath.Join(rootDir, fmt.Sprintf("after-add-%d.txt", attempt))
		if err := os.WriteFile(changedPath, []byte("watched\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(after add): %v", err)
		}
		select {
		case <-d.changeCh:
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatal("background finalization did not install the watcher")
}

func TestIntegrationCatchUpPaginatesThroughServerLimit(t *testing.T) {
	h := newIntegrationHarnessWithMaxEventFetchPage(t, 1)
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

	for _, name := range []string{"one.txt", "two.txt"} {
		if err := os.WriteFile(filepath.Join(rootDir1, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
	}
	root, err := first.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel(first): %v", err)
	}
	if err := first.rescanRoot(context.Background(), root.RootID); err != nil {
		t.Fatalf("first rescanRoot: %v", err)
	}
	if err := second.bootstrapOrCatchUp(context.Background()); err != nil {
		t.Fatalf("second bootstrapOrCatchUp: %v", err)
	}

	for _, name := range []string{"one.txt", "two.txt"} {
		got, err := os.ReadFile(filepath.Join(home2, "notes", name))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		if string(got) != name+"\n" {
			t.Fatalf("contents of %s = %q", name, string(got))
		}
	}
	st, err := second.stateDB.LoadWorkspaceState()
	if err != nil {
		t.Fatalf("LoadWorkspaceState(second): %v", err)
	}
	current, err := h.serverDB.CurrentSeq(second.cfg.WorkspaceID)
	if err != nil {
		t.Fatalf("CurrentSeq: %v", err)
	}
	if st.LastServerSeq != current {
		t.Fatalf("last server seq = %d want %d", st.LastServerSeq, current)
	}
}

func TestIntegrationSubscribesBeforeCatchUpAndLive(t *testing.T) {
	var (
		requestMu sync.Mutex
		requests  []string
	)
	h := newIntegrationHarnessWithObserver(t, 1000, func(path string) {
		if path != "/v1/ws" && path != "/v1/events" {
			return
		}
		requestMu.Lock()
		requests = append(requests, path)
		requestMu.Unlock()
	})
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancelDaemon := newTestDaemon(t)
	defer cancelDaemon()
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- d.syncAndStream(ctx)
	}()
	deadline := time.Now().Add(3 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		status, err := d.Status()
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		requestMu.Lock()
		requestCount := len(requests)
		requestMu.Unlock()
		if requestCount >= 2 && status.Connection == protocol.ConnectionLive {
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("syncAndStream did not subscribe and catch up in time")
	}
	cancel()
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("syncAndStream did not stop after cancellation")
	}

	requestMu.Lock()
	got := append([]string(nil), requests...)
	requestMu.Unlock()
	if len(got) < 2 {
		t.Fatalf("expected websocket and catch-up requests, got %v", got)
	}
	if got[0] != "/v1/ws" {
		t.Fatalf("first live-sync request = %q want /v1/ws; requests=%v", got[0], got)
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

func TestIntegrationDirectoryMTimeOnlyDoesNotPublishEcho(t *testing.T) {
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
	before, err := h.serverDB.CurrentSeq(d.cfg.WorkspaceID)
	if err != nil {
		t.Fatalf("CurrentSeq(before): %v", err)
	}

	wantMTime := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(rootDir, wantMTime, wantMTime); err != nil {
		t.Fatalf("Chtimes(root): %v", err)
	}
	if err := d.rescanRoot(context.Background(), root.RootID); err != nil {
		t.Fatalf("rescanRoot: %v", err)
	}
	after, err := h.serverDB.CurrentSeq(d.cfg.WorkspaceID)
	if err != nil {
		t.Fatalf("CurrentSeq(after): %v", err)
	}
	if after != before {
		t.Fatalf("directory mtime echo advanced server sequence from %d to %d", before, after)
	}
	entries, err := d.stateDB.EntriesForRoot(root.RootID)
	if err != nil {
		t.Fatalf("EntriesForRoot: %v", err)
	}
	if got := entries[""].MTimeNS; got != wantMTime.UnixNano() {
		t.Fatalf("local directory mtime baseline = %d want %d", got, wantMTime.UnixNano())
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

func TestIntegrationRemoveRootWithAnotherRootStillActive(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()

	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	firstDir := filepath.Join(home, "first")
	secondDir := filepath.Join(home, "second")
	for _, dir := range []string{firstDir, secondDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
		if err := d.AddRoot(context.Background(), dir); err != nil {
			t.Fatalf("AddRoot(%s): %v", dir, err)
		}
	}
	if err := d.RemoveRoot(context.Background(), firstDir); err != nil {
		t.Fatalf("RemoveRoot(first): %v", err)
	}

	first, err := d.stateDB.RootByHomeRel("first")
	if err != nil {
		t.Fatalf("RootByHomeRel(first): %v", err)
	}
	if first.State != protocol.RootStateRemoved {
		t.Fatalf("removed root state = %s want %s", first.State, protocol.RootStateRemoved)
	}
	second, err := d.stateDB.RootByHomeRel("second")
	if err != nil {
		t.Fatalf("RootByHomeRel(second): %v", err)
	}
	if second.State != protocol.RootStateActive {
		t.Fatalf("retained root state = %s want %s", second.State, protocol.RootStateActive)
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

func TestIntegrationFileConflictRetainsStageWhenRestorationFails(t *testing.T) {
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
	filePath := filepath.Join(rootDir, "note.txt")
	if err := os.WriteFile(filePath, []byte("only local copy\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(note): %v", err)
	}
	locked := false
	d.conn.HTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if !locked && req.Method == http.MethodGet && req.URL.Path == "/v1/events" {
			locked = true
			if err := os.Chmod(rootDir, 0); err != nil {
				t.Fatalf("Chmod(lock root): %v", err)
			}
		}
		return http.DefaultTransport.RoundTrip(req)
	})
	item := scanner.Entry{
		RelPath:       "note.txt",
		AbsPath:       filePath,
		Kind:          protocol.RootKindFile,
		Mode:          0o644,
		MTimeNS:       time.Now().UnixNano(),
		ContentSHA256: "local",
	}
	err := d.resolveFileConflict(context.Background(), state.Root{
		RootID:        "root-1",
		Kind:          protocol.RootKindDir,
		HomeRelPath:   "notes",
		TargetAbsPath: rootDir,
		State:         protocol.RootStateActive,
	}, item, &PathConflictError{CurrentSeq: 999})
	if chmodErr := os.Chmod(rootDir, 0o755); chmodErr != nil {
		t.Fatalf("Chmod(unlock root): %v", chmodErr)
	}
	if err == nil || !strings.Contains(err.Error(), "restore staged local file") {
		t.Fatalf("resolveFileConflict error = %v, want restoration failure", err)
	}
	matches, err := filepath.Glob(filepath.Join(d.paths.StateDir, "syna-conflict-*"))
	if err != nil {
		t.Fatalf("Glob(staged files): %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("retained staged files = %v, want one", matches)
	}
	got, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("ReadFile(retained stage): %v", err)
	}
	if string(got) != "only local copy\n" {
		t.Fatalf("retained stage contents = %q", string(got))
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
