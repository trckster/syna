package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"syna/internal/common/protocol"
	servercfg "syna/internal/server/config"
	"syna/internal/server/db"
	"syna/internal/server/hub"
	"syna/internal/server/objectstore"
)

func TestRootRendersWelcomePage(t *testing.T) {
	api, _ := newAPITestHarness(t)
	api.cfg.PublicBaseURL = "https://syna.example.com/"
	if err := api.db.AddTransferredBytes(3_000_000); err != nil {
		t.Fatalf("AddTransferredBytes: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("Content-Type = %q want text/html", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Syna",
		"Private, encrypted sync for every Linux device you trust.",
		"curl -fsSL https://raw.githubusercontent.com/trckster/syna/master/scripts/install.sh | sh",
		"syna connect https://syna.example.com",
		`syna add "$HOME/Documents"`,
		`data-copy="syna connect https://syna.example.com"`,
		"/readyz",
		"3.00 MB",
		"transferred to devices",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body does not contain %q", want)
		}
	}
	if strings.Contains(body, "/healthz") {
		t.Fatal("body contains /healthz")
	}
	removedGuidance := strings.Join([]string{"Keep the server", "behind HTTPS"}, " ")
	removedPath := strings.Join([]string{"/var", "lib", "syna"}, "/")
	if strings.Contains(body, removedGuidance) || strings.Contains(body, removedPath) {
		t.Fatal("body contains removed deployment guidance")
	}
}

func TestObjectDownloadIncrementsTransferredBytes(t *testing.T) {
	api, token := newAPITestHarness(t)
	body := []byte("encrypted object bytes")
	sum := sha256.Sum256(body)
	objectID := hex.EncodeToString(sum[:])
	if created, err := api.store.Put(api.db.SQL, objectID, "file_chunk", int64(len(body)), int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatalf("store.Put: %v", err)
	} else if !created {
		t.Fatal("expected object to be created")
	}

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/objects/"+objectID, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(protocol.VersionHeader, "1")
		rec := httptest.NewRecorder()
		api.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("download %d status = %d want %d body=%s", i, rec.Code, http.StatusOK, rec.Body.String())
		}
		if !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("download %d body mismatch", i)
		}
	}

	got, err := api.db.TransferredBytes()
	if err != nil {
		t.Fatalf("TransferredBytes: %v", err)
	}
	if want := int64(len(body) * 2); got != want {
		t.Fatalf("transferred bytes = %d want %d", got, want)
	}
}

func TestRootCatchAllReturnsNotFoundForUnknownPath(t *testing.T) {
	api, _ := newAPITestHarness(t)

	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rec := httptest.NewRecorder()

	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d want %d", rec.Code, http.StatusNotFound)
	}
}

func TestObjectUploadRejectsInvalidHeaders(t *testing.T) {
	api, token := newAPITestHarness(t)

	req := httptest.NewRequest(http.MethodPut, "/v1/objects/object-1", bytes.NewReader([]byte("blob")))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Syna-Protocol", "1")
	req.Header.Set("X-Syna-Plain-Size", "4")
	rec := httptest.NewRecorder()

	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestObjectUploadRejectsOversizedFileChunk(t *testing.T) {
	api, token := newAPITestHarness(t)

	req := httptest.NewRequest(http.MethodPut, "/v1/objects/object-1", bytes.NewReader(bytes.Repeat([]byte("a"), 32)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Syna-Protocol", "1")
	req.Header.Set("X-Syna-Object-Kind", "file_chunk")
	req.Header.Set("X-Syna-Plain-Size", "17")
	rec := httptest.NewRecorder()

	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestObjectUploadRejectsOversizedSnapshot(t *testing.T) {
	api, token := newAPITestHarness(t)

	req := httptest.NewRequest(http.MethodPut, "/v1/objects/object-1", bytes.NewReader(bytes.Repeat([]byte("a"), 32)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Syna-Protocol", "1")
	req.Header.Set("X-Syna-Object-Kind", "snapshot")
	req.Header.Set("X-Syna-Plain-Size", "33")
	rec := httptest.NewRecorder()

	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestAPIRejectsUnauthenticatedAndBadProtocolRequests(t *testing.T) {
	api, token := newAPITestHarness(t)

	cases := []struct {
		name   string
		method string
		path   string
		authz  string
		proto  string
	}{
		{name: "missing auth", method: http.MethodGet, path: "/v1/bootstrap", proto: "1"},
		{name: "bad bearer", method: http.MethodGet, path: "/v1/bootstrap", authz: "Bearer bad-token", proto: "1"},
		{name: "missing protocol", method: http.MethodGet, path: "/v1/bootstrap", authz: "Bearer " + token},
		{name: "events missing auth", method: http.MethodGet, path: "/v1/events?after_seq=0", proto: "1"},
		{name: "snapshot missing auth", method: http.MethodPost, path: "/v1/snapshots", proto: "1"},
		{name: "object missing auth", method: http.MethodGet, path: "/v1/objects/object-1", proto: "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.authz != "" {
				req.Header.Set("Authorization", tc.authz)
			}
			if tc.proto != "" {
				req.Header.Set(protocol.VersionHeader, tc.proto)
			}
			rec := httptest.NewRecorder()
			api.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d want %d", rec.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestAPIRejectsInvalidEventAndObjectRequests(t *testing.T) {
	api, token := newAPITestHarness(t)

	req := httptest.NewRequest(http.MethodPut, "/v1/objects/0000000000000000000000000000000000000000000000000000000000000000", bytes.NewReader([]byte("not matching")))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(protocol.VersionHeader, "1")
	req.Header.Set("X-Syna-Object-Kind", "file_chunk")
	req.Header.Set("X-Syna-Plain-Size", "1")
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong object hash status = %d want %d", rec.Code, http.StatusBadRequest)
	}

	assertEventStatus(t, api, token, protocol.EventSubmitRequest{
		RootID:      "root-1",
		RootKind:    protocol.RootKindDir,
		EventType:   protocol.EventRootAdd,
		PayloadBlob: "descriptor",
		ObjectRefs:  []string{"missing-object"},
	}, http.StatusBadRequest)

	assertEventStatus(t, api, token, protocol.EventSubmitRequest{
		RootID:      "root-1",
		RootKind:    protocol.RootKindDir,
		EventType:   protocol.EventRootAdd,
		PayloadBlob: "descriptor",
	}, http.StatusOK)
	assertEventStatus(t, api, token, protocol.EventSubmitRequest{
		RootID:      "root-1",
		PathID:      "path-1",
		EventType:   protocol.EventFilePut,
		BaseSeq:     ptrInt64(1),
		PayloadBlob: "file-put",
	}, http.StatusConflict)
	assertEventStatus(t, api, token, protocol.EventSubmitRequest{
		RootID:      "unknown-root",
		PathID:      "path-1",
		EventType:   protocol.EventFilePut,
		BaseSeq:     ptrInt64(0),
		PayloadBlob: "file-put",
	}, http.StatusBadRequest)
	assertEventStatus(t, api, token, protocol.EventSubmitRequest{
		RootID:      "root-1",
		EventType:   protocol.EventRootRemove,
		PayloadBlob: "remove",
	}, http.StatusOK)
	assertEventStatus(t, api, token, protocol.EventSubmitRequest{
		RootID:      "root-1",
		PathID:      "path-1",
		EventType:   protocol.EventFilePut,
		BaseSeq:     ptrInt64(0),
		PayloadBlob: "file-put",
	}, http.StatusBadRequest)

	fetch := httptest.NewRequest(http.MethodGet, "/v1/events?after_seq=0", nil)
	fetch.Header.Set("Authorization", "Bearer "+token)
	fetch.Header.Set(protocol.VersionHeader, "1")
	rec = httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, fetch)
	if rec.Code != http.StatusGone {
		t.Fatalf("fetch before retained floor status = %d want %d", rec.Code, http.StatusGone)
	}
}

func TestSessionStartRejectsMalformedWorkspacePublicKey(t *testing.T) {
	api, _ := newAPITestHarness(t)
	nonce := bytes.Repeat([]byte{0x11}, 32)

	body, err := json.Marshal(protocol.SessionStartRequest{
		WorkspaceID:     "workspace-bad-key",
		DeviceID:        "device-1",
		DeviceName:      "device",
		ClientNonce:     base64Raw(nonce),
		CreateIfMissing: true,
		WorkspacePubKey: base64Raw([]byte("short")),
	})
	if err != nil {
		t.Fatalf("Marshal(SessionStartRequest): %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/session/start", bytes.NewReader(body))
	req.Header.Set(protocol.VersionHeader, "1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if _, err := api.db.WorkspacePubKey("workspace-bad-key"); err == nil {
		t.Fatalf("malformed public key should not create a workspace")
	}
}

func TestSessionFinishWithStoredMalformedPublicKeyDoesNotPanic(t *testing.T) {
	api, _ := newAPITestHarness(t)
	if _, err := api.db.EnsureWorkspace("workspace-short-key", []byte("short")); err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	nonce := bytes.Repeat([]byte{0x22}, 32)
	serverNonce := bytes.Repeat([]byte{0x33}, 32)
	if err := api.db.SaveChallenge("workspace-short-key", "device-1", "device", nonce, serverNonce); err != nil {
		t.Fatalf("SaveChallenge: %v", err)
	}

	body, err := json.Marshal(protocol.SessionFinishRequest{
		WorkspaceID: "workspace-short-key",
		DeviceID:    "device-1",
		ClientNonce: base64Raw(nonce),
		ServerNonce: base64Raw(serverNonce),
		Signature:   base64Raw(bytes.Repeat([]byte{0x44}, ed25519.SignatureSize)),
	})
	if err != nil {
		t.Fatalf("Marshal(SessionFinishRequest): %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/session/finish", bytes.NewReader(body))
	req.Header.Set(protocol.VersionHeader, "1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d want %d body=%s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

func TestSessionHandshakeAcceptsValidWorkspacePublicKey(t *testing.T) {
	api, _ := newAPITestHarness(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	nonce := bytes.Repeat([]byte{0x55}, 32)

	body, err := json.Marshal(protocol.SessionStartRequest{
		WorkspaceID:     "workspace-good-key",
		DeviceID:        "device-1",
		DeviceName:      "device",
		ClientNonce:     base64Raw(nonce),
		CreateIfMissing: true,
		WorkspacePubKey: base64Raw(pub),
	})
	if err != nil {
		t.Fatalf("Marshal(SessionStartRequest): %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/session/start", bytes.NewReader(body))
	req.Header.Set(protocol.VersionHeader, "1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func newAPITestHarness(t *testing.T) (*API, string) {
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
	if _, err := database.EnsureWorkspace("workspace-1", []byte("pubkey")); err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	token, _, _, err := database.CreateSession("workspace-1", "device-1", "device", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	cfg := servercfg.Config{
		DataDir:           dataDir,
		SessionTTL:        time.Hour,
		EventRetention:    24 * time.Hour,
		ZeroRefRetention:  24 * time.Hour,
		MaxEventFetchPage: 1000,
		MaxPlainChunkSize: 16,
		MaxEventBodyBytes: 1 << 20,
		MaxSnapshotBody:   64,
		MaxSnapshotPlain:  32,
		MaxWSClients:      2,
	}
	logger := log.New(io.Discard, "", 0)
	return New(cfg, database, objectstore.New(dataDir), hub.New(cfg.MaxWSClients, logger), logger), token
}

func assertEventStatus(t *testing.T, api *API, token string, event protocol.EventSubmitRequest, want int) {
	t.Helper()
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("Marshal(event): %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/events", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(protocol.VersionHeader, "1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != want {
		t.Fatalf("event %+v status = %d want %d body=%s", event, rec.Code, want, rec.Body.String())
	}
}

func ptrInt64(v int64) *int64 {
	return &v
}

func base64Raw(b []byte) string {
	return strings.TrimRight(base64.StdEncoding.EncodeToString(b), "=")
}
