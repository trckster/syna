package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"syna/internal/client/agentrpc"
	"syna/internal/client/uploader"
	"syna/internal/common/protocol"
)

func TestConnectRPCProgress(t *testing.T) {
	for _, scenario := range []string{"snapshots", "event replay", "file root replay", "empty workspace", "empty files and directories", "download failure"} {
		t.Run(scenario, func(t *testing.T) {
			h := newIntegrationHarnessWithMaxEventFetchPage(t, 2)
			defer h.Close()
			home1 := filepath.Join(t.TempDir(), "source")
			setHome(t, home1)
			first, cancel := newTestDaemon(t)
			defer cancel()
			created, err := first.Connect(context.Background(), ConnectRequest{ServerURL: h.serverURL})
			if err != nil {
				t.Fatal(err)
			}
			contents := bytes.Repeat([]byte("x"), uploader.ChunkSize+17)
			if scenario == "empty files and directories" {
				contents = nil
			}
			roots := []string{"notes", "docs"}
			files := []string{"notes/file", "docs/file"}
			if scenario == "file root replay" {
				roots = []string{"standalone"}
				files = []string{"standalone"}
			}
			if scenario != "empty workspace" {
				for i, name := range roots {
					root := filepath.Join(home1, name)
					if scenario != "file root replay" {
						if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.WriteFile(filepath.Join(home1, files[i]), contents, 0o644); err != nil {
						t.Fatal(err)
					}
					if err := first.AddRoot(context.Background(), root); err != nil {
						t.Fatal(err)
					}
				}
			}
			replayOnly := scenario == "event replay" || scenario == "file root replay"
			if replayOnly {
				// Simulate a workspace whose initial events are still retained
				// and have not yet been covered by snapshots.
				if _, err := h.serverDB.SQL.Exec(`UPDATE roots SET latest_snapshot_object_id = NULL, latest_snapshot_seq = NULL`); err != nil {
					t.Fatal(err)
				}
				if _, err := h.serverDB.SQL.Exec(`UPDATE workspaces SET retained_floor_seq = 0`); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "download failure" {
				if err := os.RemoveAll(filepath.Join(h.dataDir, "objects")); err != nil {
					t.Fatal(err)
				}
			}
			home2 := filepath.Join(t.TempDir(), "destination")
			setHome(t, home2)
			second, cancelSecond := newTestDaemon(t)
			defer cancelSecond()

			// Exercise the real daemon RPC stream, including its final response.
			client, server := net.Pipe()
			defer func() { _ = client.Close() }()
			_ = client.SetDeadline(time.Now().Add(30 * time.Second))
			go second.handleConn(server)
			if err := json.NewEncoder(client).Encode(agentrpc.Request{
				Method: "connect", Progress: true,
				Params: agentrpc.EncodeResult(ConnectRequest{ServerURL: h.serverURL, RecoveryKey: created.GeneratedRecoveryKey}),
			}); err != nil {
				t.Fatal(err)
			}
			var updates []agentrpc.Progress
			var result agentrpc.Response
			decoder := json.NewDecoder(client)
			for {
				result = agentrpc.Response{}
				if err := decoder.Decode(&result); err != nil {
					t.Fatal(err)
				}
				if result.Progress == nil {
					break
				}
				updates = append(updates, *result.Progress)
			}
			if len(updates) == 0 || updates[0].Stage != "connecting" {
				t.Fatalf("missing immediate progress: %+v", updates)
			}
			previous := 0.0
			chunkProgress, entryProgress := false, false
			replayPaths := make(map[string]bool)
			for _, update := range updates {
				if update.Fraction == nil || *update.Fraction < previous || *update.Fraction > 1 {
					t.Fatalf("invalid or regressing progress: %+v (previous %f)", update, previous)
				}
				previous = *update.Fraction
				if update.Stage != "done" && previous >= 1 {
					t.Fatal("reported completion before success")
				}
				if update.DoneBytes > 0 && update.DoneBytes < update.TotalBytes {
					chunkProgress = true
					if update.Stage == "catching_up" {
						replayPaths[update.Path] = true
					}
				}
				if update.Stage == "downloading" && update.TotalEntries > 0 && update.DoneEntries == update.TotalEntries {
					entryProgress = true
				}
			}
			last := updates[len(updates)-1]
			if scenario == "download failure" {
				if result.OK || result.Error == "" || last.Stage == "done" {
					t.Fatalf("failed download reported success: %+v, last progress %+v", result, last)
				}
				return
			}
			if !result.OK || last.Stage != "done" || *last.Fraction != 1 {
				t.Fatalf("missing successful completion: %+v, last progress %+v", result, last)
			}
			var response ConnectResponse
			if err := json.Unmarshal(result.Result, &response); err != nil || response.WorkspaceID != created.WorkspaceID {
				t.Fatalf("connect response = %+v, err = %v", response, err)
			}
			if scenario == "empty workspace" {
				return
			}
			if (scenario == "snapshots" || replayOnly) && !chunkProgress {
				t.Fatal("missing progress during a multi-chunk file download")
			}
			if scenario == "empty files and directories" && !entryProgress {
				t.Fatal("zero-byte entries did not advance progress")
			}
			for _, name := range files {
				if replayOnly && !replayPaths[name] {
					t.Errorf("replay progress missing workspace-relative path %q; got %v", name, replayPaths)
				}
				got, err := os.ReadFile(filepath.Join(home2, name))
				if err != nil || !bytes.Equal(got, contents) {
					t.Fatalf("restored %s differs, err = %v", name, err)
				}
			}
		})
	}
}

func TestConnectProgressEstimateDoesNotRegress(t *testing.T) {
	var updates []AddProgress
	p := &connectProgress{emit: func(update AddProgress) { updates = append(updates, update) }}
	p.startCatchUp(0, 10)
	p.finishEvent(8)
	before := p.fraction
	// The workspace grows while this client is catching up.
	p.startCatchUp(8, 100)
	if p.fraction < before {
		t.Fatal("new events moved progress backwards")
	}
	// A retention gap can require starting bootstrap again.
	p.startRoot(0, 2)
	p.planSnapshot(protocol.SnapshotPayload{})
	if p.fraction < before {
		t.Fatal("re-bootstrap moved progress backwards")
	}
	if updates[0].Fraction == updates[len(updates)-1].Fraction {
		t.Fatal("updates share a mutable fraction")
	}
}

func TestConnectProgressReplayAllocation(t *testing.T) {
	p := &connectProgress{emit: func(AddProgress) {}}
	p.planRoots([]protocol.BootstrapRoot{{RootID: "without-snapshot"}})
	p.startCatchUp(0, 10)
	if p.fraction > 0.05 {
		t.Fatalf("replay-only restore jumped to %f before downloading", p.fraction)
	}
	p.finishEvent(5)
	if p.fraction < 0.5 || p.fraction > 0.6 {
		t.Fatalf("halfway through replay: fraction = %f", p.fraction)
	}
}
