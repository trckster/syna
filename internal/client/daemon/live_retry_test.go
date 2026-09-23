package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"syna/internal/client/connector"
	"syna/internal/common/protocol"
)

func TestLiveStreamRetriesPendingChangesWithoutStatus(t *testing.T) {
	for _, operation := range []string{"upload", "event", "root_remove"} {
		t.Run(operation, func(t *testing.T) {
			h := newRestartableHarness(t)
			defer h.Close()
			h.Stop()
			var failing atomic.Bool
			var websockets, attempts atomic.Int32
			next := h.handler
			h.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/ws" {
					websockets.Add(1)
				}
				failPath := r.Method == http.MethodPut
				if operation != "upload" {
					failPath = r.Method == http.MethodPost && r.URL.Path == "/v1/events"
				}
				if failPath && failing.Load() {
					attempts.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"code":"temporarily_unavailable","message":"please try again"}`))
					return
				}
				next.ServeHTTP(w, r)
			})
			h.Start(t)
			home := filepath.Join(t.TempDir(), "home")
			setHome(t, home)
			d, cancelDaemon := newTestDaemon(t)
			defer cancelDaemon()
			if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
				t.Fatal(err)
			}
			rootDir := filepath.Join(home, "notes")
			if err := os.MkdirAll(rootDir, 0o755); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(rootDir, "note.txt")
			if err := os.WriteFile(file, []byte("before\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := d.AddRoot(context.Background(), rootDir); err != nil {
				t.Fatal(err)
			}
			root, err := d.stateDB.RootByHomeRel("notes")
			if err != nil {
				t.Fatal(err)
			}
			beforeSeq, err := h.serverDB.CurrentSeq(d.cfg.WorkspaceID)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); d.reconnectLoop(ctx) }()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("reconnect loop did not stop")
				}
			}()
			waitForRetryCondition(t, func() bool { return d.isStreamingLive() })
			failing.Store(true)
			if operation == "root_remove" {
				if err := os.RemoveAll(rootDir); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(file, []byte("after\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			d.syncMu.Lock()
			err = d.rescanRootHint(context.Background(), root.RootID, "")
			d.syncMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			waitForRetryCondition(t, func() bool {
				ops, err := d.stateDB.ListPendingOps()
				return err == nil && len(ops) == 1 && ops[0].RetryCount > 0
			})
			failing.Store(false)
			waitForRetryCondition(t, func() bool {
				pending, err := d.stateDB.CountPendingOps()
				if err != nil || pending != 0 {
					return false
				}
				seq, err := h.serverDB.CurrentSeq(d.cfg.WorkspaceID)
				if err != nil || seq <= beforeSeq {
					return false
				}
				st, err := d.stateDB.LoadWorkspaceState()
				return err == nil && st.ConnectionState == protocol.ConnectionLive && st.LastError == ""
			})
			if got := websockets.Load(); got != 1 {
				t.Fatalf("WebSocket reconnects masked retry: got %d connections", got)
			}
			if got := attempts.Load(); got < 2 {
				t.Fatalf("expected initial failure and a background retry, got %d", got)
			}
		})
	}
}

func TestLivePendingRetriesRespectBackoff(t *testing.T) {
	h := newIntegrationHarness(t)
	defer h.Close()
	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()
	if _, err := d.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, "note.txt")
	if err := os.WriteFile(file, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.AddRoot(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	root, err := d.stateDB.RootByHomeRel("note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	var attempts int
	d.conn.HTTPClient.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return nil, errors.New("connection reset by peer")
	})
	if err := d.rescanRoot(context.Background(), root.RootID); err != nil {
		t.Fatal(err)
	}
	if err := d.retryLivePendingOps(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts=%d, want initial request and first retry", attempts)
	}
	ops, err := d.stateDB.ListPendingOps()
	if err != nil || len(ops) != 1 {
		t.Fatalf("pending operations: %v %v", ops, err)
	}
	// Pin the retry in the future so a slow test host cannot cross the backoff boundary.
	if err := d.stateDB.BumpPendingOpRetry(ops[0].OpID, "connection reset", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := d.retryLivePendingOps(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if attempts != 2 {
		t.Fatalf("retried before next_retry_at: %d attempts", attempts)
	}
	st, err := d.stateDB.LoadWorkspaceState()
	if err != nil || st.ConnectionState != protocol.ConnectionDegraded {
		t.Fatalf("pending failure hidden: %+v %v", st, err)
	}
	ctx, stop := context.WithCancel(context.Background())
	stop()
	if err := d.retryLivePendingOps(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled retry = %v", err)
	}
}

func TestRetryableHTTPStatuses(t *testing.T) {
	for _, code := range []int{408, 429, 500, 502, 503, 504} {
		if !isRetryableSyncError(fmt.Errorf("wrapped: %w", &connector.HTTPError{StatusCode: code, Code: "failure", Message: "please try again"})) {
			t.Errorf("HTTP %d should retry", code)
		}
	}
	for _, code := range []int{400, 403, 404, 409, 413} {
		if isRetryableSyncError(&connector.HTTPError{StatusCode: code, Message: "rejected"}) {
			t.Errorf("HTTP %d should not retry", code)
		}
	}
}

func waitForRetryCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background retry condition was not reached")
}
