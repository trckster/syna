package agentrpc

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
)

func TestCallWithProgressStreamsUpdatesBeforeFinalResponse(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "agent.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		defer conn.Close()

		var req Request
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			t.Errorf("Decode request: %v", err)
			return
		}
		if req.Method != "add" || !req.Progress {
			t.Errorf("request = %+v, want method add with progress", req)
			return
		}
		enc := json.NewEncoder(conn)
		if err := enc.Encode(Response{OK: true, Progress: &Progress{Stage: "syncing", DoneBytes: 5, TotalBytes: 10}}); err != nil {
			t.Errorf("Encode progress: %v", err)
			return
		}
		if err := enc.Encode(Response{OK: true, Result: EncodeResult(map[string]string{"status": "ok"})}); err != nil {
			t.Errorf("Encode final: %v", err)
		}
	}()

	var gotProgress []Progress
	var resp map[string]string
	if err := CallWithProgress(socketPath, "add", nil, &resp, func(progress Progress) {
		gotProgress = append(gotProgress, progress)
	}); err != nil {
		t.Fatalf("CallWithProgress: %v", err)
	}
	<-done

	if len(gotProgress) != 1 {
		t.Fatalf("progress count = %d, want 1", len(gotProgress))
	}
	if gotProgress[0].Stage != "syncing" || gotProgress[0].DoneBytes != 5 || gotProgress[0].TotalBytes != 10 {
		t.Fatalf("progress = %+v", gotProgress[0])
	}
	if resp["status"] != "ok" {
		t.Fatalf("response = %+v", resp)
	}
}
