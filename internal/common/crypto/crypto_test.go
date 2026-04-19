package crypto

import (
	"bytes"
	"testing"
)

func TestRecoveryKeyRoundTrip(t *testing.T) {
	display, raw, err := GenerateRecoveryKey()
	if err != nil {
		t.Fatalf("GenerateRecoveryKey: %v", err)
	}
	parsed, err := ParseRecoveryKey(display)
	if err != nil {
		t.Fatalf("ParseRecoveryKey: %v", err)
	}
	if !bytes.Equal(parsed, raw) {
		t.Fatalf("parsed key mismatch")
	}
}

func TestEncryptDecrypt(t *testing.T) {
	_, raw, err := GenerateRecoveryKey()
	if err != nil {
		t.Fatalf("GenerateRecoveryKey: %v", err)
	}
	keys, err := Derive(raw)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	aad := EventAAD("workspace", "root", "path", "file_put")
	blob, err := Encrypt(keys.EventKey, []byte("hello"), aad)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := Decrypt(keys.EventKey, blob, aad)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("unexpected plaintext %q", string(got))
	}
}

func TestDeterministicIDs(t *testing.T) {
	raw := bytes.Repeat([]byte{0x42}, 32)
	keys, err := Derive(raw)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	workspaceID1 := WorkspaceID(keys)
	workspaceID2 := WorkspaceID(keys)
	if workspaceID1 != workspaceID2 {
		t.Fatalf("workspace IDs differ: %q vs %q", workspaceID1, workspaceID2)
	}
	rootID1 := RootID(keys, "notes")
	rootID2 := RootID(keys, "notes")
	if rootID1 != rootID2 {
		t.Fatalf("root IDs differ: %q vs %q", rootID1, rootID2)
	}
	pathID1 := PathID(keys, rootID1, "notes/today.md")
	pathID2 := PathID(keys, rootID1, "notes/today.md")
	if pathID1 != pathID2 {
		t.Fatalf("path IDs differ: %q vs %q", pathID1, pathID2)
	}
}

func TestVerifyTranscript(t *testing.T) {
	raw := bytes.Repeat([]byte{0x24}, 32)
	keys, err := Derive(raw)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	privateKey, publicKey := AuthKeys(keys)
	clientNonce := []byte("client-nonce-12345678901234567890")
	serverNonce := []byte("server-nonce-12345678901234567890")
	signature := SignTranscript(privateKey, "workspace", "device", clientNonce, serverNonce)
	if !VerifyTranscript(publicKey, "workspace", "device", clientNonce, serverNonce, signature) {
		t.Fatalf("expected transcript verification to succeed")
	}
	if VerifyTranscript(publicKey, "workspace", "other-device", clientNonce, serverNonce, signature) {
		t.Fatalf("expected transcript verification to fail for mismatched device")
	}
	if VerifyTranscript(publicKey[:1], "workspace", "device", clientNonce, serverNonce, signature) {
		t.Fatalf("expected transcript verification to fail for malformed public key")
	}
	if VerifyTranscript(publicKey, "workspace", "device", clientNonce, serverNonce, signature[:1]) {
		t.Fatalf("expected transcript verification to fail for malformed signature")
	}
}
