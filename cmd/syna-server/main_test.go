package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"syna/internal/client/connector"
	commoncrypto "syna/internal/common/crypto"
	"syna/internal/common/protocol"
)

func TestServerProcessSessionEventsBootstrapAndWebSocket(t *testing.T) {
	bin := buildServerBinary(t)
	addr := freeTCPAddr(t)
	dataDir := shortServerTempDir(t, "data")

	logFile, err := os.Create(filepath.Join(shortServerTempDir(t, "logs"), "server.log"))
	if err != nil {
		t.Fatalf("Create(server.log): %v", err)
	}
	defer logFile.Close()

	cmd := exec.Command(bin, "serve")
	cmd.Env = append(filteredServerEnv("SYNA_LISTEN", "SYNA_DATA_DIR", "SYNA_ALLOW_HTTP", "SYNA_PUBLIC_BASE_URL"),
		"SYNA_LISTEN="+addr,
		"SYNA_DATA_DIR="+dataDir,
		"SYNA_ALLOW_HTTP=true",
		"SYNA_PUBLIC_BASE_URL=http://"+addr,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start syna-server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	baseURL := "http://" + addr
	waitForHTTP(t, baseURL+"/readyz", 5*time.Second)
	waitForHTTP(t, baseURL+"/healthz", 5*time.Second)

	conn, workspaceID := authenticatedConnector(t, baseURL)
	ws, err := conn.DialWS(context.Background())
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer ws.Close()
	time.Sleep(100 * time.Millisecond)

	rootAdd, _, err := conn.SubmitEvent(context.Background(), protocol.EventSubmitRequest{
		RootID:      "root-1",
		RootKind:    protocol.RootKindDir,
		EventType:   protocol.EventRootAdd,
		PayloadBlob: "descriptor",
	})
	if err != nil {
		t.Fatalf("SubmitEvent(root_add): %v", err)
	}
	dirPut, _, err := conn.SubmitEvent(context.Background(), protocol.EventSubmitRequest{
		RootID:      "root-1",
		PathID:      "path-1",
		EventType:   protocol.EventDirPut,
		BaseSeq:     ptrInt64(0),
		PayloadBlob: "dir-put",
	})
	if err != nil {
		t.Fatalf("SubmitEvent(dir_put): %v", err)
	}

	events, _, err := conn.FetchEvents(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("FetchEvents: %v", err)
	}
	if len(events.Events) != 2 || events.Events[0].Seq != rootAdd.AcceptedSeq || events.Events[1].Seq != dirPut.AcceptedSeq {
		t.Fatalf("unexpected event ordering: %+v", events.Events)
	}

	bootstrap, err := conn.Bootstrap(context.Background())
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if bootstrap.WorkspaceID != workspaceID || len(bootstrap.Roots) != 1 || bootstrap.Roots[0].RootID != "root-1" {
		t.Fatalf("unexpected bootstrap response: %+v", bootstrap)
	}

	var msg protocol.WSMessage
	if err := ws.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if err := ws.ReadJSON(&msg); err != nil {
		t.Fatalf("ReadJSON(ws): %v", err)
	}
	if msg.Type != "event" || msg.Event == nil || msg.Event.Seq != rootAdd.AcceptedSeq {
		t.Fatalf("unexpected websocket message: %+v", msg)
	}
}

func buildServerBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "syna-server")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/syna-server")
	cmd.Dir = filepath.Clean(filepath.Join("..", ".."))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build ./cmd/syna-server: %v\n%s", err, string(out))
	}
	return bin
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func shortServerTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "syna-server-"+prefix+"-")
	if err != nil {
		t.Fatalf("MkdirTemp(%s): %v", prefix, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func filteredServerEnv(keys ...string) []string {
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

func waitForHTTP(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not become ready: %v", url, lastErr)
}

func authenticatedConnector(t *testing.T, baseURL string) (*connector.Client, string) {
	t.Helper()
	_, rawKey, err := commoncrypto.GenerateRecoveryKey()
	if err != nil {
		t.Fatalf("GenerateRecoveryKey: %v", err)
	}
	keys, err := commoncrypto.Derive(rawKey)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	workspaceID := commoncrypto.WorkspaceID(keys)
	privateKey, publicKey := commoncrypto.AuthKeys(keys)
	clientNonce := make([]byte, 32)
	if _, err := rand.Read(clientNonce); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	base := connector.New(baseURL)
	startResp, err := base.SessionStart(context.Background(), protocol.SessionStartRequest{
		WorkspaceID:     workspaceID,
		DeviceID:        "device-1",
		DeviceName:      "process-test",
		ClientNonce:     commoncrypto.Base64Raw(clientNonce),
		CreateIfMissing: true,
		WorkspacePubKey: commoncrypto.Base64Raw(publicKey),
	})
	if err != nil {
		t.Fatalf("SessionStart: %v", err)
	}
	serverNonce, err := commoncrypto.ParseBase64Raw(startResp.ServerNonce)
	if err != nil {
		t.Fatalf("ParseBase64Raw(server nonce): %v", err)
	}
	signature := commoncrypto.SignTranscript(privateKey, workspaceID, "device-1", clientNonce, serverNonce)
	finishResp, err := base.SessionFinish(context.Background(), protocol.SessionFinishRequest{
		WorkspaceID: workspaceID,
		DeviceID:    "device-1",
		ClientNonce: commoncrypto.Base64Raw(clientNonce),
		ServerNonce: startResp.ServerNonce,
		Signature:   commoncrypto.Base64Raw(signature),
	})
	if err != nil {
		t.Fatalf("SessionFinish: %v", err)
	}
	return base.WithToken(finishResp.SessionToken), workspaceID
}

func ptrInt64(v int64) *int64 {
	return &v
}
