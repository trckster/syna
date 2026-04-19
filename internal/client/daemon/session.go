package daemon

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"syna/internal/client/connector"
	commoncrypto "syna/internal/common/crypto"
	"syna/internal/common/protocol"
)

type sessionHandshake struct {
	ServerURL       string
	WorkspaceID     string
	DeviceID        string
	DeviceName      string
	Keys            *commoncrypto.DerivedKeys
	CreateIfMissing bool
}

type authenticatedSession struct {
	Client     *connector.Client
	Token      string
	ExpiresAt  time.Time
	CurrentSeq int64
}

func authenticateSession(ctx context.Context, req sessionHandshake) (*authenticatedSession, error) {
	if req.Keys == nil {
		return nil, errors.New("workspace key unavailable")
	}
	privateKey, publicKey := commoncrypto.AuthKeys(req.Keys)
	clientNonce, err := randomNonce()
	if err != nil {
		return nil, err
	}
	client := connector.New(req.ServerURL)
	startResp, err := client.SessionStart(ctx, protocol.SessionStartRequest{
		WorkspaceID:     req.WorkspaceID,
		DeviceID:        req.DeviceID,
		DeviceName:      req.DeviceName,
		ClientNonce:     commoncrypto.Base64Raw(clientNonce),
		CreateIfMissing: req.CreateIfMissing,
		WorkspacePubKey: commoncrypto.Base64Raw(publicKey),
	})
	if err != nil {
		return nil, err
	}
	serverNonce, err := commoncrypto.ParseBase64Raw(startResp.ServerNonce)
	if err != nil {
		return nil, err
	}
	signature := commoncrypto.SignTranscript(privateKey, req.WorkspaceID, req.DeviceID, clientNonce, serverNonce)
	finishResp, err := client.SessionFinish(ctx, protocol.SessionFinishRequest{
		WorkspaceID: req.WorkspaceID,
		DeviceID:    req.DeviceID,
		ClientNonce: commoncrypto.Base64Raw(clientNonce),
		ServerNonce: startResp.ServerNonce,
		Signature:   commoncrypto.Base64Raw(signature),
	})
	if err != nil {
		return nil, err
	}
	return &authenticatedSession{
		Client:     client.WithToken(finishResp.SessionToken),
		Token:      finishResp.SessionToken,
		ExpiresAt:  finishResp.ExpiresAt,
		CurrentSeq: finishResp.CurrentSeq,
	}, nil
}

func randomNonce() ([]byte, error) {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return nonce, nil
}
