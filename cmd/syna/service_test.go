package main

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"syna/internal/client/agentrpc"
	"syna/internal/client/configstore"
	commoncfg "syna/internal/common/config"
	"syna/internal/common/protocol"
)

func TestEnsureSocketPreservesReachableDaemon(t *testing.T) {
	for _, response := range []string{"slow", "error", "invalid"} {
		t.Run(response, func(t *testing.T) {
			home := shortTempDir(t, "probe")
			paths := clientPaths(home)
			if err := commoncfg.EnsureClientDirs(paths); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", paths.SocketFile)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			original, err := os.Stat(paths.SocketFile)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				switch response {
				case "slow":
					time.Sleep(700 * time.Millisecond)
				case "error":
					_ = json.NewEncoder(conn).Encode(agentrpc.Response{Error: "database is busy"})
				case "invalid":
					_, _ = conn.Write([]byte("invalid\n"))
				}
			}()
			t.Setenv("PATH", t.TempDir())
			if _, err := ensureSocket(paths); err == nil || !strings.Contains(err.Error(), "daemon status probe failed") {
				t.Fatalf("unexpected error: %v", err)
			}
			current, err := os.Stat(paths.SocketFile)
			if err != nil || !os.SameFile(original, current) {
				t.Fatalf("live socket replaced or removed: %v", err)
			}
			<-done
		})
	}
}

func TestCLIStatusRepairsDisabledService(t *testing.T) {
	bin := buildSynaBinary(t)
	server := newCLITestServer(t)
	defer server.Close()
	home := shortTempDir(t, "enable")
	t.Cleanup(func() { stopDaemon(t, home) })
	if out, stderr, err := runSyna(t, bin, home, "\n", "connect", server.URL); err != nil {
		t.Fatalf("connect: %v\n%s\n%s", err, out, stderr)
	}
	enabled := filepath.Join(home, ".config", "systemd", "user", "syna.enabled")
	if err := os.Remove(enabled); err != nil {
		t.Fatal(err)
	}
	pidBefore, err := os.ReadFile(clientPaths(home).PIDFile)
	if err != nil {
		t.Fatal(err)
	}
	out, stderr, err := runSyna(t, bin, home, "", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, stderr)
	}
	if _, err := os.Stat(enabled); err != nil {
		t.Fatalf("status did not repair login startup: %v", err)
	}
	pidAfter, err := os.ReadFile(clientPaths(home).PIDFile)
	if err != nil || string(pidBefore) != string(pidAfter) {
		t.Fatalf("status restarted running daemon: %v", err)
	}
	var status protocol.WorkspaceStatus
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatal(err)
	}
	if status.DaemonVersion == "" {
		t.Fatal("status omitted running daemon version")
	}
	stopDaemon(t, home)
	if err := os.Remove(enabled); err != nil {
		t.Fatal(err)
	}
	if out, stderr, err := runSyna(t, bin, home, "", "status"); err != nil {
		t.Fatalf("status after stop: %v\n%s\n%s", err, out, stderr)
	}
	if _, err := os.Stat(enabled); err != nil {
		t.Fatalf("cold startup did not enable service: %v", err)
	}
}

func TestCLIServiceRefresh(t *testing.T) {
	bin := buildSynaBinary(t)
	for _, tc := range []struct {
		name            string
		auto, connected bool
		want            string
	}{
		{"connected", true, true, "--user daemon-reload\n--user enable syna.service\n--user restart syna.service\n"},
		{"manual", false, true, "--user daemon-reload\n--user restart syna.service\n"},
		{"unconfigured", true, false, "--user daemon-reload\n--user restart syna.service\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := shortTempDir(t, "refresh")
			paths := clientPaths(home)
			cfg := configstore.Config{DaemonAutoStart: tc.auto}
			if tc.connected {
				cfg.ServerURL = "https://example.test"
				cfg.WorkspaceID = "workspace"
			}
			if err := configstore.New(paths).SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			env := clientEnvWithSystemctl(t, home, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HOME/calls\"\n")
			if out, stderr, err := runSynaWithEnv(t, bin, home, env, "", "service", "refresh"); err != nil {
				t.Fatalf("refresh: %v\n%s\n%s", err, out, stderr)
			}
			calls, err := os.ReadFile(filepath.Join(home, "calls"))
			if err != nil || string(calls) != tc.want {
				t.Fatalf("systemctl calls = %q, err=%v, want %q", calls, err, tc.want)
			}
			unit, err := os.ReadFile(paths.UnitFile)
			if err != nil || !strings.Contains(string(unit), "ExecStart="+bin+" daemon") {
				t.Fatalf("unit does not use upgrading binary: %s, %v", unit, err)
			}
		})
	}
}

func TestCLIStatusWarnsAboutOlderDaemon(t *testing.T) {
	bin := buildSynaBinary(t)
	home := shortTempDir(t, "old-daemon")
	paths := clientPaths(home)
	if err := commoncfg.EnsureClientDirs(paths); err != nil {
		t.Fatal(err)
	}
	if err := configstore.New(paths).SaveConfig(configstore.Config{DaemonAutoStart: false}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", paths.SocketFile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 2 {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			var req agentrpc.Request
			_ = json.NewDecoder(conn).Decode(&req)
			_ = json.NewEncoder(conn).Encode(agentrpc.Response{OK: true, Result: agentrpc.EncodeResult(protocol.WorkspaceStatus{Connection: protocol.ConnectionLive})})
			_ = conn.Close()
		}
	}()
	stdout, stderr, err := runSyna(t, bin, home, "", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, stderr)
	}
	var status protocol.WorkspaceStatus
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Warnings) != 1 || !strings.Contains(status.Warnings[0], "syna service refresh") {
		t.Fatalf("missing old daemon warning: %+v", status)
	}
	<-done
}

func TestEnsureSocketKeepsSocketAfterStartupTimeout(t *testing.T) {
	home := shortTempDir(t, "slow-start")
	paths := clientPaths(home)
	if err := commoncfg.EnsureClientDirs(paths); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	script := "#!/bin/sh\nif [ \"$2\" = start ]; then touch \"$TEST_STARTED\"; fi\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(home, "started")
	t.Setenv("TEST_STARTED", started)
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))
	ready := make(chan net.Listener, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(started); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		listener, err := net.Listen("unix", paths.SocketFile)
		if err != nil {
			ready <- nil
			return
		}
		ready <- listener
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, err := ensureSocket(paths)
	listener := <-ready
	if listener == nil {
		t.Fatal("test listener failed")
	}
	defer func() { _ = listener.Close(); <-done }()
	if err == nil || !strings.Contains(err.Error(), "service started but daemon did not answer") {
		t.Fatalf("unexpected startup error: %v", err)
	}
	if _, err := os.Stat(paths.SocketFile); err != nil {
		t.Fatalf("startup timeout removed socket: %v", err)
	}
}

func TestCLIRefreshReplacesRunningDevelopmentBuild(t *testing.T) {
	original := buildSynaBinary(t)
	installDir := shortTempDir(t, "upgrade-bin")
	installed := filepath.Join(installDir, "syna")
	image, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, image, 0o755); err != nil {
		t.Fatal(err)
	}
	server := newCLITestServer(t)
	defer server.Close()
	home := shortTempDir(t, "upgrade-home")
	t.Cleanup(func() { stopDaemon(t, home) })
	if out, stderr, err := runSyna(t, installed, home, "\n", "connect", server.URL); err != nil {
		t.Fatalf("connect: %v\n%s\n%s", err, out, stderr)
	}
	before := readCLIStatus(t, installed, home)
	replacement := filepath.Join(installDir, "syna-new")
	build := exec.Command("go", "build", "-ldflags=-buildid=syna-upgrade-regression", "-o", replacement, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build replacement: %v\n%s", err, output)
	}
	if err := os.Rename(replacement, installed); err != nil {
		t.Fatal(err)
	}
	stale := readCLIStatus(t, installed, home)
	if stale.DaemonExecutableID != before.DaemonExecutableID || stale.DaemonExecutableID == "" {
		t.Fatalf("running image identity changed when its path was replaced: before=%+v after=%+v", before, stale)
	}
	if !strings.Contains(strings.Join(stale.Warnings, "\n"), "differs from CLI") {
		t.Fatalf("different development executables were not detected: %+v", stale)
	}
	if out, stderr, err := runSyna(t, installed, home, "", "service", "refresh"); err != nil {
		t.Fatalf("refresh: %v\n%s\n%s", err, out, stderr)
	}
	after := readCLIStatus(t, installed, home)
	if after.DaemonVersion != before.DaemonVersion {
		t.Fatalf("test requires identical version metadata: before=%q after=%q", before.DaemonVersion, after.DaemonVersion)
	}
	if after.DaemonExecutableID == before.DaemonExecutableID {
		t.Fatal("service refresh retained old executable")
	}
	if strings.Contains(strings.Join(after.Warnings, "\n"), "differs from CLI") {
		t.Fatalf("refreshed daemon still mismatches CLI: %+v", after)
	}
}

func readCLIStatus(t *testing.T, bin, home string) protocol.WorkspaceStatus {
	t.Helper()
	stdout, stderr, err := runSyna(t, bin, home, "", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, stderr)
	}
	var status protocol.WorkspaceStatus
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatal(err)
	}
	return status
}
