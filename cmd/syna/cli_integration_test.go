package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

func TestCLIHelpDoesNotNeedDaemon(t *testing.T) {
	bin := buildSynaBinary(t)
	home := shortTempDir(t, "home")

	stdout, _, err := runSyna(t, bin, home, "", "help")
	if err != nil {
		t.Fatalf("syna help: %v", err)
	}
	for _, want := range []string{
		"  syna connect <server-url>  connect",
		"  syna disconnect            disconnect",
		"  syna key show              print",
		"  syna add <path>            add",
		"  syna rm <path>             stop syncing",
		"  syna status                print",
		"  syna uninstall             remove",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("help output missing %q:\n%s", want, stdout)
		}
	}
	for _, hidden := range []string{
		"syna -h",
		"syna --help",
		"syna --version",
	} {
		if strings.Contains(stdout, hidden) {
			t.Fatalf("help output should not list alias %q:\n%s", hidden, stdout)
		}
	}
	if _, err := os.Stat(clientPaths(home).SocketFile); !os.IsNotExist(err) {
		t.Fatalf("help should not create or contact daemon socket, stat err=%v", err)
	}
}

func TestCLIFreshConnectAutoStartsDaemonAndInitializesClient(t *testing.T) {
	bin := buildSynaBinary(t)
	server := newCLITestServer(t)
	defer server.Close()

	home := shortTempDir(t, "home")
	t.Cleanup(func() { stopDaemon(t, home) })

	stdout, stderr, err := runSyna(t, bin, home, "\n", "connect", server.URL)
	if err != nil {
		t.Fatalf("syna connect failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if recoveryKey(stdout) == "" {
		t.Fatalf("connect output did not include generated recovery key:\n%s", stdout)
	}
	if !strings.Contains(stdout, "You can show it again on this connected device with: syna key show") {
		t.Fatalf("connect output did not mention syna key show:\n%s", stdout)
	}
	paths := clientPaths(home)
	for _, path := range []string{paths.ConfigFile, paths.KeyringFile, paths.DBFile, paths.SocketFile, paths.PIDFile} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected %s to exist after connect: %v", path, err)
		}
	}
	var status protocol.WorkspaceStatus
	stdout, stderr, err = runSyna(t, bin, home, "", "status")
	if err != nil {
		t.Fatalf("syna status failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatalf("decode status: %v\n%s", err, stdout)
	}
	if status.WorkspaceID == "" || status.ServerURL != server.URL {
		t.Fatalf("unexpected status after connect: %+v", status)
	}
}

func TestCLIKeyShowReadsLocalKeyring(t *testing.T) {
	bin := buildSynaBinary(t)
	server := newCLITestServer(t)
	defer server.Close()

	home := shortTempDir(t, "key-home")
	t.Cleanup(func() { stopDaemon(t, home) })

	stdout, stderr, err := runSyna(t, bin, home, "", "key", "show")
	if err == nil {
		t.Fatalf("expected key show before connect to fail\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "no recovery key is stored; connect to a workspace first") {
		t.Fatalf("unexpected key show failure before connect\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	stdout, stderr, err = runSyna(t, bin, home, "\n", "connect", server.URL)
	if err != nil {
		t.Fatalf("connect: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	key := recoveryKey(stdout)
	if key == "" {
		t.Fatalf("missing recovery key in output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Anyone with it can access the encrypted workspace; store it safely.") {
		t.Fatalf("connect output missing recovery key warning:\n%s", stdout)
	}

	stopDaemon(t, home)
	paths := clientPaths(home)
	_ = os.Remove(paths.SocketFile)
	_ = os.Remove(paths.PIDFile)
	stdout, stderr, err = runSyna(t, bin, home, "", "key", "show")
	if err != nil {
		t.Fatalf("key show without daemon: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if stdout != key+"\n" {
		t.Fatalf("key show stdout = %q want %q", stdout, key+"\n")
	}
	if stderr != "" {
		t.Fatalf("key show should not write stderr, got %q", stderr)
	}
	if _, err := os.Stat(paths.SocketFile); !os.IsNotExist(err) {
		t.Fatalf("key show should not create or contact daemon socket, stat err=%v", err)
	}
	if _, err := os.Stat(paths.PIDFile); !os.IsNotExist(err) {
		t.Fatalf("key show should not start daemon, pid file stat err=%v", err)
	}

	if stdout, stderr, err = runSyna(t, bin, home, "", "disconnect"); err != nil {
		t.Fatalf("disconnect: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "Disconnected this device. Local files were left untouched.") {
		t.Fatalf("disconnect output missing confirmation:\n%s", stdout)
	}
	stdout, stderr, err = runSyna(t, bin, home, "", "key", "show")
	if err == nil {
		t.Fatalf("expected key show after disconnect to fail\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "no recovery key is stored; connect to a workspace first") {
		t.Fatalf("unexpected key show failure after disconnect\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
}

func TestCLIKeyUsageRejectsUnsupportedForms(t *testing.T) {
	bin := buildSynaBinary(t)
	home := shortTempDir(t, "key-usage")

	for _, args := range [][]string{
		{"key"},
		{"key", "list"},
		{"key", "show", "extra"},
	} {
		stdout, stderr, err := runSyna(t, bin, home, "", args...)
		if err == nil {
			t.Fatalf("expected syna %s to fail\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), stdout, stderr)
		}
		requireExitCode(t, err, 2)
		if !strings.Contains(stdout, "  syna key show              print the stored workspace recovery key") {
			t.Fatalf("usage for syna %s missing key show\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), stdout, stderr)
		}
		if stderr != "" {
			t.Fatalf("usage error should not write stderr, got %q", stderr)
		}
	}
	if _, err := os.Stat(clientPaths(home).SocketFile); !os.IsNotExist(err) {
		t.Fatalf("unsupported key forms should not create or contact daemon socket, stat err=%v", err)
	}
}

func TestCLIUninstallRemovesSynaDataButLeavesSyncedDirectories(t *testing.T) {
	bin := buildSynaBinary(t)
	server := newCLITestServer(t)
	defer server.Close()

	home := shortTempDir(t, "uninstall-home")
	t.Cleanup(func() { stopDaemon(t, home) })

	root := filepath.Join(home, "documents")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	note := filepath.Join(root, "note.txt")
	if err := os.WriteFile(note, []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(note): %v", err)
	}

	stdout, stderr, err := runSyna(t, bin, home, "\n", "connect", server.URL)
	if err != nil {
		t.Fatalf("connect: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if stdout, stderr, err = runSyna(t, bin, home, "", "add", root); err != nil {
		t.Fatalf("add: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	paths := clientPaths(home)
	if _, err := os.Stat(paths.UnitFile); err != nil {
		t.Fatalf("expected unit file before uninstall: %v", err)
	}

	stdout, stderr, err = runSyna(t, bin, home, "", "uninstall")
	if err != nil {
		t.Fatalf("uninstall: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "Synced directories were left untouched.") {
		t.Fatalf("uninstall output missing synced-directory confirmation:\n%s", stdout)
	}
	if _, err := os.Stat(note); err != nil {
		t.Fatalf("uninstall should leave synced file untouched: %v", err)
	}
	for _, path := range []string{paths.ConfigDir, paths.StateDir, paths.UnitFile, bin} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be removed, stat err=%v", path, err)
		}
	}
}

func TestCLIRealDaemonsSyncCreateEditAndDelete(t *testing.T) {
	bin := buildSynaBinary(t)
	server := newCLITestServer(t)
	defer server.Close()

	homeA := shortTempDir(t, "home-a")
	homeB := shortTempDir(t, "home-b")
	homeC := shortTempDir(t, "home-c")
	t.Cleanup(func() {
		stopDaemon(t, homeA)
		stopDaemon(t, homeB)
		stopDaemon(t, homeC)
	})

	rootA := filepath.Join(homeA, "notes")
	if err := os.MkdirAll(filepath.Join(rootA, "deep"), 0o755); err != nil {
		t.Fatalf("MkdirAll(rootA): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootA, "deep", "note.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(initial): %v", err)
	}

	stdout, stderr, err := runSyna(t, bin, homeA, "\n", "connect", server.URL)
	if err != nil {
		t.Fatalf("connect A: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	key := recoveryKey(stdout)
	if key == "" {
		t.Fatalf("missing recovery key in output:\n%s", stdout)
	}
	if stdout, stderr, err = runSyna(t, bin, homeA, "", "add", rootA); err != nil {
		t.Fatalf("add A: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	if stdout, stderr, err = runSyna(t, bin, homeB, key+"\n", "connect", server.URL); err != nil {
		t.Fatalf("connect B: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	rootB := filepath.Join(homeB, "notes")
	waitForFileContent(t, filepath.Join(rootB, "deep", "note.txt"), "initial\n", 10*time.Second)

	if err := os.WriteFile(filepath.Join(rootA, "deep", "note.txt"), []byte("edited\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(edited): %v", err)
	}
	waitForFileContent(t, filepath.Join(rootB, "deep", "note.txt"), "edited\n", 15*time.Second)

	if err := os.WriteFile(filepath.Join(rootA, "created.txt"), []byte("created\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(created): %v", err)
	}
	waitForFileContent(t, filepath.Join(rootB, "created.txt"), "created\n", 15*time.Second)

	if err := os.Remove(filepath.Join(rootA, "created.txt")); err != nil {
		t.Fatalf("Remove(created): %v", err)
	}
	waitForMissing(t, filepath.Join(rootB, "created.txt"), 15*time.Second)

	if stdout, stderr, err = runSyna(t, bin, homeA, "", "rm", rootA); err != nil {
		t.Fatalf("rm A: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	waitForRootState(t, bin, homeB, protocol.RootStateRemoved, 15*time.Second)
	if _, err := os.Stat(filepath.Join(rootB, "deep", "note.txt")); err != nil {
		t.Fatalf("root remove should leave B's local files untouched: %v", err)
	}

	if err := os.WriteFile(filepath.Join(rootA, "deep", "note.txt"), []byte("readded\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(readded): %v", err)
	}
	if stdout, stderr, err = runSyna(t, bin, homeA, "", "add", rootA); err != nil {
		t.Fatalf("re-add A: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if stdout, stderr, err = runSyna(t, bin, homeC, key+"\n", "connect", server.URL); err != nil {
		t.Fatalf("connect C after re-add: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	waitForFileContent(t, filepath.Join(homeC, "notes", "deep", "note.txt"), "readded\n", 10*time.Second)

	if stdout, stderr, err = runSyna(t, bin, homeA, "", "disconnect"); err != nil {
		t.Fatalf("disconnect A: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "Disconnected this device. Local files were left untouched.") {
		t.Fatalf("disconnect output missing confirmation:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(rootA, "deep", "note.txt")); err != nil {
		t.Fatalf("disconnect should leave A's local files untouched: %v", err)
	}
	status := statusFromCLI(t, bin, homeA)
	if status.Connection != protocol.ConnectionDisconnected || len(status.TrackedRoots) != 0 {
		t.Fatalf("unexpected status after disconnect: %+v", status)
	}
}

func TestCLIRealDaemonsConflictCopyConverges(t *testing.T) {
	bin := buildSynaBinary(t)
	server := newRestartableCLITestServer(t)
	defer server.Close()

	homeA := shortTempDir(t, "conflict-a")
	homeB := shortTempDir(t, "conflict-b")
	t.Cleanup(func() {
		stopDaemon(t, homeA)
		stopDaemon(t, homeB)
	})

	rootA := filepath.Join(homeA, "notes")
	if err := os.MkdirAll(rootA, 0o755); err != nil {
		t.Fatalf("MkdirAll(rootA): %v", err)
	}
	noteA := filepath.Join(rootA, "note.txt")
	if err := os.WriteFile(noteA, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(base): %v", err)
	}

	stdout, stderr, err := runSyna(t, bin, homeA, "\n", "connect", server.URL)
	if err != nil {
		t.Fatalf("connect A: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	key := recoveryKey(stdout)
	if key == "" {
		t.Fatalf("missing recovery key in output:\n%s", stdout)
	}
	if stdout, stderr, err = runSyna(t, bin, homeA, "", "add", rootA); err != nil {
		t.Fatalf("add A: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if stdout, stderr, err = runSyna(t, bin, homeB, key+"\n", "connect", server.URL); err != nil {
		t.Fatalf("connect B: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	rootB := filepath.Join(homeB, "notes")
	noteB := filepath.Join(rootB, "note.txt")
	waitForFileContent(t, noteB, "base\n", 10*time.Second)

	server.Stop()
	if err := os.WriteFile(noteB, []byte("from-b\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(B): %v", err)
	}
	waitForPendingOps(t, bin, homeB, 1, 10*time.Second)
	stopDaemon(t, homeB)
	server.Start(t)
	if err := os.WriteFile(noteA, []byte("from-a\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(A): %v", err)
	}
	time.Sleep(2 * time.Second)

	if stdout, stderr, err = runSyna(t, bin, homeB, "", "status"); err != nil {
		t.Fatalf("restart/status B: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	waitForFileContent(t, noteB, "from-a\n", 20*time.Second)
	waitForConflictContent(t, rootB, "from-b\n", 20*time.Second)
	waitForConflictContent(t, rootA, "from-b\n", 20*time.Second)
}

func TestCLIPathValidationRejectsUnsafeRoots(t *testing.T) {
	bin := buildSynaBinary(t)
	server := newCLITestServer(t)
	defer server.Close()

	home := shortTempDir(t, "path-home")
	t.Cleanup(func() { stopDaemon(t, home) })
	stdout, stderr, err := runSyna(t, bin, home, "\n", "connect", server.URL)
	if err != nil {
		t.Fatalf("connect: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	outside := shortTempDir(t, "outside")
	if stdout, stderr, err = runSyna(t, bin, home, "", "add", outside); err == nil {
		t.Fatalf("expected outside-home add to fail\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	realRoot := filepath.Join(home, "real")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(real): %v", err)
	}
	linkRoot := filepath.Join(home, "link-root")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if stdout, stderr, err = runSyna(t, bin, home, "", "add", linkRoot); err == nil {
		t.Fatalf("expected symlink root add to fail\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	parent := filepath.Join(home, "projects")
	child := filepath.Join(parent, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("MkdirAll(child): %v", err)
	}
	if stdout, stderr, err = runSyna(t, bin, home, "", "add", parent); err != nil {
		t.Fatalf("add parent: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if stdout, stderr, err = runSyna(t, bin, home, "", "add", child); err == nil {
		t.Fatalf("expected overlapping child add to fail\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
}

func buildSynaBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "syna")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/syna")
	cmd.Dir = filepath.Clean(filepath.Join("..", ".."))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build ./cmd/syna: %v\n%s", err, string(out))
	}
	return bin
}

func shortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "syna-"+prefix+"-")
	if err != nil {
		t.Fatalf("MkdirTemp(%s): %v", prefix, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func newCLITestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "server")
	if err := servercfg.EnsureDataDirs(dataDir); err != nil {
		t.Fatalf("EnsureDataDirs: %v", err)
	}
	database, err := db.Open(filepath.Join(dataDir, "state.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
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
		MaxWSClients:      16,
	}
	return httptest.NewServer(api.New(cfg, database, objectstore.New(dataDir), hub.New(cfg.MaxWSClients, log.New(io.Discard, "", 0)), log.New(io.Discard, "", 0)).Handler())
}

type cliRestartableServer struct {
	URL      string
	serverDB *db.DB
	handler  http.Handler
	addr     string
	server   *http.Server
}

func newRestartableCLITestServer(t *testing.T) *cliRestartableServer {
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
		MaxWSClients:      16,
	}
	server := &cliRestartableServer{
		serverDB: database,
		handler:  api.New(cfg, database, objectstore.New(dataDir), hub.New(cfg.MaxWSClients, log.New(io.Discard, "", 0)), log.New(io.Discard, "", 0)).Handler(),
	}
	server.Start(t)
	return server
}

func (s *cliRestartableServer) Start(t *testing.T) {
	t.Helper()
	addr := s.addr
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
	s.addr = ln.Addr().String()
	s.URL = "http://" + s.addr
	s.server = &http.Server{Handler: s.handler}
	go func() {
		if err := s.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("restartable server stopped: %v", err)
		}
	}()
}

func (s *cliRestartableServer) Stop() {
	if s != nil && s.server != nil {
		_ = s.server.Close()
		s.server = nil
	}
}

func (s *cliRestartableServer) Close() {
	if s == nil {
		return
	}
	s.Stop()
	_ = s.serverDB.Close()
}

func runSyna(t *testing.T, bin, home, stdin string, args ...string) (string, string, error) {
	t.Helper()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("MkdirAll(home): %v", err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = clientEnv(t, home)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func requireExitCode(t *testing.T, err error, want int) {
	t.Helper()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error %v is not an exit error", err)
	}
	if got := exitErr.ExitCode(); got != want {
		t.Fatalf("exit code = %d want %d", got, want)
	}
}

func clientEnv(t *testing.T, home string) []string {
	t.Helper()
	fakeBin := filepath.Join(t.TempDir(), "fake-bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatalf("MkdirAll(fake-bin): %v", err)
	}
	systemctl := filepath.Join(fakeBin, "systemctl")
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(systemctl): %v", err)
	}
	env := filteredEnv("HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "SYNA_ALLOW_HTTP", "PATH")
	env = append(env,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local", "state"),
		"SYNA_ALLOW_HTTP=true",
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	return env
}

func filteredEnv(keys ...string) []string {
	block := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		block[key] = struct{}{}
	}
	var out []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if _, ok := block[key]; ok {
			continue
		}
		out = append(out, item)
	}
	return out
}

func clientPaths(home string) commoncfg.ClientPaths {
	configDir := filepath.Join(home, ".config", "syna")
	stateDir := filepath.Join(home, ".local", "state", "syna")
	return commoncfg.ClientPaths{
		ConfigDir:   configDir,
		StateDir:    stateDir,
		ConfigFile:  filepath.Join(configDir, "config.json"),
		KeyringFile: filepath.Join(configDir, "keyring.json"),
		DBFile:      filepath.Join(stateDir, "client.db"),
		SocketFile:  filepath.Join(stateDir, "agent.sock"),
		PIDFile:     filepath.Join(stateDir, "daemon.pid"),
		SystemdDir:  filepath.Join(home, ".config", "systemd", "user"),
		UnitFile:    filepath.Join(home, ".config", "systemd", "user", "syna.service"),
	}
}

func recoveryKey(output string) string {
	re := regexp.MustCompile(`syna1-[0-9a-f]{64}-[0-9a-f]{8}`)
	return re.FindString(output)
}

func stopDaemon(t *testing.T, home string) {
	t.Helper()
	pidBytes, err := os.ReadFile(clientPaths(home).PIDFile)
	if err != nil {
		return
	}
	pid := strings.TrimSpace(string(pidBytes))
	if pid == "" {
		return
	}
	_ = exec.Command("kill", pid).Run()
	for i := 0; i < 50; i++ {
		if err := exec.Command("kill", "-0", pid).Run(); err != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForFileContent(t *testing.T, path, want string, timeout time.Duration) {
	t.Helper()
	waitUntil(t, timeout, func() (bool, string) {
		got, err := os.ReadFile(path)
		if err != nil {
			return false, err.Error()
		}
		if string(got) != want {
			return false, fmt.Sprintf("content %q", string(got))
		}
		return true, ""
	})
}

func waitForMissing(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	waitUntil(t, timeout, func() (bool, string) {
		_, err := os.Stat(path)
		if os.IsNotExist(err) {
			return true, ""
		}
		return false, fmt.Sprintf("stat err=%v", err)
	})
}

func waitForConflictContent(t *testing.T, root, want string, timeout time.Duration) {
	t.Helper()
	waitUntil(t, timeout, func() (bool, string) {
		matches, err := filepath.Glob(filepath.Join(root, "note.syna-conflict-*"))
		if err != nil {
			return false, err.Error()
		}
		if len(matches) != 1 {
			return false, fmt.Sprintf("conflicts=%v", matches)
		}
		body, err := os.ReadFile(matches[0])
		if err != nil {
			return false, err.Error()
		}
		if string(body) != want {
			return false, fmt.Sprintf("conflict content %q", string(body))
		}
		return true, ""
	})
}

func waitForPendingOps(t *testing.T, bin, home string, want int64, timeout time.Duration) {
	t.Helper()
	waitUntil(t, timeout, func() (bool, string) {
		status := statusFromCLI(t, bin, home)
		if status.PendingOps == want {
			return true, ""
		}
		return false, fmt.Sprintf("pending_ops=%d last_error=%s", status.PendingOps, status.LastError)
	})
}

func waitForRootState(t *testing.T, bin, home string, want protocol.RootState, timeout time.Duration) {
	t.Helper()
	waitUntil(t, timeout, func() (bool, string) {
		status := statusFromCLI(t, bin, home)
		for _, root := range status.TrackedRoots {
			if root.State == want {
				return true, ""
			}
		}
		return false, fmt.Sprintf("roots=%+v", status.TrackedRoots)
	})
}

func statusFromCLI(t *testing.T, bin, home string) protocol.WorkspaceStatus {
	t.Helper()
	stdout, stderr, err := runSyna(t, bin, home, "", "status")
	if err != nil {
		t.Fatalf("status: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	var status protocol.WorkspaceStatus
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatalf("decode status: %v\n%s", err, stdout)
	}
	return status
}

func waitUntil(t *testing.T, timeout time.Duration, fn func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var detail string
	for time.Now().Before(deadline) {
		ok, current := fn()
		if ok {
			return
		}
		detail = current
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, detail)
}
