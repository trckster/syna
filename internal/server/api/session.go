package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	commoncrypto "syna/internal/common/crypto"
	"syna/internal/common/protocol"
)

const sessionNonceSize = 32

func (a *API) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}
	var req protocol.SessionStartRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if req.WorkspaceID == "" {
		writeError(w, http.StatusBadRequest, "bad_workspace_id", "workspace_id is required")
		return
	}
	if req.DeviceID == "" {
		writeError(w, http.StatusBadRequest, "bad_device_id", "device_id is required")
		return
	}
	clientNonce, err := parseFixedBase64(req.ClientNonce, sessionNonceSize, "client_nonce")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_client_nonce", err.Error())
		return
	}
	var pubKey []byte
	if req.WorkspacePubKey != "" {
		pubKey, err = commoncrypto.ParseBase64Raw(req.WorkspacePubKey)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_workspace_pubkey", err.Error())
			return
		}
		if len(pubKey) != ed25519.PublicKeySize {
			writeError(w, http.StatusBadRequest, "bad_workspace_pubkey", fmt.Sprintf("workspace_pubkey must be %d bytes", ed25519.PublicKeySize))
			return
		}
	}
	created := false
	_, err = a.db.WorkspacePubKey(req.WorkspaceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if !req.CreateIfMissing {
				writeError(w, http.StatusNotFound, "workspace_not_found", "")
				return
			}
			if len(pubKey) != ed25519.PublicKeySize {
				writeError(w, http.StatusBadRequest, "bad_workspace_pubkey", "workspace_pubkey is required when creating a workspace")
				return
			}
			created, err = a.db.EnsureWorkspace(req.WorkspaceID, pubKey)
			if err != nil {
				writeError(w, http.StatusBadRequest, "workspace_create_failed", err.Error())
				return
			}
		} else {
			writeError(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
	} else {
		created, err = a.db.EnsureWorkspace(req.WorkspaceID, pubKey)
		if err != nil {
			writeError(w, http.StatusBadRequest, "workspace_key_mismatch", err.Error())
			return
		}
	}

	serverNonce := make([]byte, 32)
	if _, err := rand.Read(serverNonce); err != nil {
		writeError(w, http.StatusInternalServerError, "random_failed", err.Error())
		return
	}
	if err := a.db.SaveChallenge(req.WorkspaceID, req.DeviceID, req.DeviceName, clientNonce, serverNonce); err != nil {
		writeError(w, http.StatusInternalServerError, "challenge_save_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, protocol.SessionStartResponse{
		WorkspaceExists: true,
		Created:         created,
		ServerNonce:     commoncrypto.Base64Raw(serverNonce),
		ServerTime:      time.Now().UTC(),
		ProtocolVersion: 1,
	})
}

func (a *API) handleSessionFinish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}
	var req protocol.SessionFinishRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if req.WorkspaceID == "" {
		writeError(w, http.StatusBadRequest, "bad_workspace_id", "workspace_id is required")
		return
	}
	if req.DeviceID == "" {
		writeError(w, http.StatusBadRequest, "bad_device_id", "device_id is required")
		return
	}
	clientNonce, err := parseFixedBase64(req.ClientNonce, sessionNonceSize, "client_nonce")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_client_nonce", err.Error())
		return
	}
	serverNonce, deviceName, expiresAt, err := a.db.LoadChallenge(req.WorkspaceID, req.DeviceID, clientNonce)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "challenge_not_found", "")
		return
	}
	if time.Now().UTC().After(expiresAt) {
		writeError(w, http.StatusUnauthorized, "challenge_expired", "")
		return
	}
	expectedServerNonce, err := parseFixedBase64(req.ServerNonce, sessionNonceSize, "server_nonce")
	if err != nil || string(expectedServerNonce) != string(serverNonce) {
		writeError(w, http.StatusUnauthorized, "server_nonce_mismatch", "")
		return
	}
	pubKey, err := a.db.WorkspacePubKey(req.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "workspace_not_found", "")
		return
	}
	if len(pubKey) != ed25519.PublicKeySize {
		writeError(w, http.StatusUnauthorized, "bad_workspace_pubkey", "")
		return
	}
	signature, err := parseFixedBase64(req.Signature, ed25519.SignatureSize, "signature")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_signature", err.Error())
		return
	}
	if !commoncrypto.VerifyTranscript(pubKey, req.WorkspaceID, req.DeviceID, clientNonce, serverNonce, signature) {
		writeError(w, http.StatusUnauthorized, "bad_signature", "")
		return
	}
	_ = a.db.DeleteChallenge(req.WorkspaceID, req.DeviceID, clientNonce)
	token, expires, currentSeq, err := a.db.CreateSession(req.WorkspaceID, req.DeviceID, deviceName, a.cfg.SessionTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_create_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, protocol.SessionFinishResponse{
		SessionToken: token,
		ExpiresAt:    expires,
		WorkspaceID:  req.WorkspaceID,
		CurrentSeq:   currentSeq,
	})
}

func parseFixedBase64(value string, size int, label string) ([]byte, error) {
	raw, err := commoncrypto.ParseBase64Raw(value)
	if err != nil {
		return nil, err
	}
	if len(raw) != size {
		return nil, fmt.Errorf("%s must be %d bytes", label, size)
	}
	return raw, nil
}
