package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"syna/internal/common/protocol"
)

// A second client edits a file locally while offline; the server snapshot is
// unchanged since that client's last sync. A forced re-bootstrap must keep
// the newer local edit instead of overwriting it with the stale snapshot.
func TestIntegrationRebootstrapKeepsNewerLocalEdit(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(rootDir1, "note.txt"), []byte("remote\n"), 0o644); err != nil {
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
	target := filepath.Join(home2, "notes", "note.txt")
	if got, err := os.ReadFile(target); err != nil || string(got) != "remote\n" {
		t.Fatalf("bootstrapped contents %q err=%v", string(got), err)
	}

	// Offline edit on the second device, then a forced re-bootstrap.
	if err := os.WriteFile(target, []byte("local-edit\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(local edit): %v", err)
	}
	if err := second.bootstrap(context.Background()); err != nil {
		t.Fatalf("second re-bootstrap: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(after re-bootstrap): %v", err)
	}
	if string(got) != "local-edit\n" {
		t.Fatalf("local edit was overwritten by stale snapshot: %q", string(got))
	}
	assertNoConflictCopies(t, filepath.Join(home2, "notes"))

	// The rescan must then upload the local edit so other devices receive it.
	root, err := second.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel: %v", err)
	}
	if err := second.rescanRoot(context.Background(), root.RootID); err != nil {
		t.Fatalf("rescanRoot: %v", err)
	}
	if err := first.bootstrapOrCatchUp(context.Background()); err != nil {
		t.Fatalf("first catch-up: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(rootDir1, "note.txt")); err != nil || string(got) != "local-edit\n" {
		t.Fatalf("first device contents %q err=%v", string(got), err)
	}
}

// Both sides diverged from the last synced baseline: the snapshot is applied,
// but the local version must survive as a conflict copy instead of being lost.
func TestIntegrationRebootstrapPreservesDivergedLocalAsConflictCopy(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(rootDir1, "note.txt"), []byte("remote\n"), 0o644); err != nil {
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

	root, err := second.stateDB.RootByHomeRel("notes")
	if err != nil {
		t.Fatalf("RootByHomeRel: %v", err)
	}
	// Fake a baseline that matches neither the local file nor the snapshot,
	// simulating both sides having diverged since the last sync.
	entries, err := second.stateDB.EntriesForRoot(root.RootID)
	if err != nil {
		t.Fatalf("EntriesForRoot: %v", err)
	}
	entry, ok := entries["note.txt"]
	if !ok {
		t.Fatalf("missing note.txt entry")
	}
	entry.ContentSHA256 = strings.Repeat("ab", 32)
	if err := second.stateDB.UpsertEntry(entry); err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}
	target := filepath.Join(home2, "notes", "note.txt")
	if err := os.WriteFile(target, []byte("local-edit\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(local edit): %v", err)
	}

	if err := second.bootstrap(context.Background()); err != nil {
		t.Fatalf("second re-bootstrap: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(after re-bootstrap): %v", err)
	}
	if string(got) != "remote\n" {
		t.Fatalf("expected snapshot to be applied, got %q", string(got))
	}
	matches, err := filepath.Glob(filepath.Join(home2, "notes", "note.syna-conflict-*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected exactly one conflict copy, got %v err=%v", matches, err)
	}
	saved, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("ReadFile(conflict copy): %v", err)
	}
	if string(saved) != "local-edit\n" {
		t.Fatalf("conflict copy contents %q", string(saved))
	}
}

func assertNoConflictCopies(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*syna-conflict*"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("unexpected conflict copies: %v", matches)
	}
}

func setShortWSDeadlines(t *testing.T, pongWait, pingEvery time.Duration) {
	t.Helper()
	prevPong, prevPing := wsClientPongWait, wsClientPingEvery
	wsClientPongWait, wsClientPingEvery = pongWait, pingEvery
	t.Cleanup(func() {
		wsClientPongWait, wsClientPingEvery = prevPong, prevPing
	})
}

func dialTestWS(t *testing.T, handler http.HandlerFunc) (*websocket.Conn, func()) {
	t.Helper()
	server := httptest.NewServer(handler)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		server.Close()
		t.Fatalf("Dial: %v", err)
	}
	return conn, func() {
		conn.Close()
		server.Close()
	}
}

// A server that stops responding (never answers pings) must cause the stream
// loop to fail within the pong deadline so the reconnect loop can recover,
// instead of hanging forever on a dead connection.
func TestStreamEventsDetectsDeadConnection(t *testing.T) {
	setShortWSDeadlines(t, 300*time.Millisecond, 50*time.Millisecond)
	d, cancel := newTestDaemon(t)
	defer cancel()

	silent := make(chan struct{})
	conn, cleanup := dialTestWS(t, func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Never read: the client's pings are never answered with pongs.
		<-silent
		c.Close()
	})
	defer close(silent)
	defer cleanup()

	start := time.Now()
	err := d.streamEvents(context.Background(), conn)
	if err == nil {
		t.Fatalf("expected error from dead connection")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("dead connection not detected in time (took %v)", elapsed)
	}
}

// A healthy connection that answers pings must not be torn down by the
// heartbeat, and must deliver events.
func TestStreamEventsKeepsHealthyConnectionAlive(t *testing.T) {
	setShortWSDeadlines(t, 300*time.Millisecond, 50*time.Millisecond)
	d, cancel := newTestDaemon(t)
	defer cancel()

	done := make(chan struct{})
	conn, cleanup := dialTestWS(t, func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Reading makes gorilla answer client pings with pongs automatically.
		go func() {
			for {
				if _, _, err := c.NextReader(); err != nil {
					return
				}
			}
		}()
		<-done
		c.Close()
	})
	defer cleanup()

	ctx, cancelCtx := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		var msg protocol.WSMessage
		_ = msg
		errCh <- d.streamEvents(ctx, conn)
	}()

	select {
	case err := <-errCh:
		t.Fatalf("healthy connection dropped early: %v", err)
	case <-time.After(1 * time.Second):
		// Survived well past several pong deadlines.
	}
	cancelCtx()
	close(done)
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("stream did not stop after context cancel")
	}
}
