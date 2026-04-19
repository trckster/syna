package crypto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	recoveryPrefix         = "syna1-"
	blobFormatVersion byte = 1
)

type DerivedKeys struct {
	WorkspaceIDKey []byte
	PathIDKey      []byte
	BlobKey        []byte
	EventKey       []byte
	SnapshotKey    []byte
	AuthSeed       []byte
}

func GenerateRecoveryKey() (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	return FormatRecoveryKey(raw), raw, nil
}

func FormatRecoveryKey(raw []byte) string {
	sum := sha256.Sum256(raw)
	return recoveryPrefix + hex.EncodeToString(raw) + "-" + hex.EncodeToString(sum[:4])
}

func ParseRecoveryKey(display string) ([]byte, error) {
	if !strings.HasPrefix(display, recoveryPrefix) {
		return nil, errors.New("invalid recovery key prefix")
	}
	parts := strings.Split(display, "-")
	if len(parts) != 3 {
		return nil, errors.New("invalid recovery key shape")
	}
	raw, err := hex.DecodeString(parts[1])
	if err != nil || len(raw) != 32 {
		return nil, errors.New("invalid recovery key bytes")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:4]) != parts[2] {
		return nil, errors.New("invalid recovery key checksum")
	}
	return raw, nil
}

func Derive(raw []byte) (*DerivedKeys, error) {
	if len(raw) != 32 {
		return nil, fmt.Errorf("expected 32-byte recovery key, got %d", len(raw))
	}
	read := func(info string) ([]byte, error) {
		key := make([]byte, 32)
		r := hkdf.New(sha256.New, raw, []byte("syna-v1"), []byte(info))
		if _, err := io.ReadFull(r, key); err != nil {
			return nil, err
		}
		return key, nil
	}
	workspaceIDKey, err := read("workspace-id")
	if err != nil {
		return nil, err
	}
	pathIDKey, err := read("path-id")
	if err != nil {
		return nil, err
	}
	blobKey, err := read("blob")
	if err != nil {
		return nil, err
	}
	eventKey, err := read("event")
	if err != nil {
		return nil, err
	}
	snapshotKey, err := read("snapshot")
	if err != nil {
		return nil, err
	}
	authSeed, err := read("auth-ed25519-seed")
	if err != nil {
		return nil, err
	}
	return &DerivedKeys{
		WorkspaceIDKey: workspaceIDKey,
		PathIDKey:      pathIDKey,
		BlobKey:        blobKey,
		EventKey:       eventKey,
		SnapshotKey:    snapshotKey,
		AuthSeed:       authSeed,
	}, nil
}

func WorkspaceID(k *DerivedKeys) string {
	mac := hmac.New(sha256.New, k.WorkspaceIDKey)
	mac.Write([]byte("workspace"))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

func RootID(k *DerivedKeys, homeRelPath string) string {
	mac := hmac.New(sha256.New, k.PathIDKey)
	mac.Write([]byte("root"))
	mac.Write([]byte{0})
	mac.Write([]byte(homeRelPath))
	return hex.EncodeToString(mac.Sum(nil))
}

func PathID(k *DerivedKeys, rootID, relPath string) string {
	mac := hmac.New(sha256.New, k.PathIDKey)
	mac.Write([]byte(rootID))
	mac.Write([]byte{0})
	mac.Write([]byte(relPath))
	return hex.EncodeToString(mac.Sum(nil))
}

func AuthKeys(k *DerivedKeys) (ed25519.PrivateKey, ed25519.PublicKey) {
	privateKey := ed25519.NewKeyFromSeed(k.AuthSeed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return privateKey, publicKey
}

func TranscriptDigest(workspaceID, deviceID string, clientNonce, serverNonce []byte) []byte {
	buf := bytes.NewBuffer(nil)
	buf.WriteString("syna-auth-v1")
	buf.WriteByte(0)
	buf.WriteString(workspaceID)
	buf.WriteByte(0)
	buf.WriteString(deviceID)
	buf.WriteByte(0)
	buf.Write(clientNonce)
	buf.WriteByte(0)
	buf.Write(serverNonce)
	sum := sha256.Sum256(buf.Bytes())
	return sum[:]
}

func SignTranscript(privateKey ed25519.PrivateKey, workspaceID, deviceID string, clientNonce, serverNonce []byte) []byte {
	return ed25519.Sign(privateKey, TranscriptDigest(workspaceID, deviceID, clientNonce, serverNonce))
}

func VerifyTranscript(publicKey ed25519.PublicKey, workspaceID, deviceID string, clientNonce, serverNonce, signature []byte) bool {
	if len(publicKey) != ed25519.PublicKeySize || len(signature) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(publicKey, TranscriptDigest(workspaceID, deviceID, clientNonce, serverNonce), signature)
}

func Encrypt(key, plaintext, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	body := aead.Seal(nil, nonce, plaintext, aad)
	blob := make([]byte, 1+len(nonce)+len(body))
	blob[0] = blobFormatVersion
	copy(blob[1:], nonce)
	copy(blob[1+len(nonce):], body)
	return blob, nil
}

func Decrypt(key, blob, aad []byte) ([]byte, error) {
	if len(blob) < 1+chacha20poly1305.NonceSizeX {
		return nil, errors.New("blob too short")
	}
	if blob[0] != blobFormatVersion {
		return nil, fmt.Errorf("unsupported blob version %d", blob[0])
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := blob[1 : 1+chacha20poly1305.NonceSizeX]
	ciphertext := blob[1+chacha20poly1305.NonceSizeX:]
	return aead.Open(nil, nonce, ciphertext, aad)
}

func DecryptInPlace(key, blob, aad []byte) ([]byte, error) {
	if len(blob) < 1+chacha20poly1305.NonceSizeX {
		return nil, errors.New("blob too short")
	}
	if blob[0] != blobFormatVersion {
		return nil, fmt.Errorf("unsupported blob version %d", blob[0])
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := blob[1 : 1+chacha20poly1305.NonceSizeX]
	ciphertext := blob[1+chacha20poly1305.NonceSizeX:]
	return aead.Open(ciphertext[:0], nonce, ciphertext, aad)
}

func DecryptToWriter(key, blob, aad []byte, dst io.Writer) (int64, error) {
	plain, err := DecryptInPlace(key, blob, aad)
	if err != nil {
		return 0, err
	}
	n, err := dst.Write(plain)
	if err != nil {
		return int64(n), err
	}
	if n != len(plain) {
		return int64(n), io.ErrShortWrite
	}
	return int64(n), nil
}

func ObjectID(blob []byte) string {
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

func EncryptedSize(plainSize int64) (int64, error) {
	if plainSize < 0 {
		return 0, fmt.Errorf("negative plain size %d", plainSize)
	}
	return plainSize + 1 + chacha20poly1305.NonceSizeX + chacha20poly1305.Overhead, nil
}

func Base64Raw(bytes []byte) string {
	return base64.RawStdEncoding.EncodeToString(bytes)
}

func ParseBase64Raw(s string) ([]byte, error) {
	return base64.RawStdEncoding.DecodeString(s)
}

func BlobAAD(workspaceID, rootID, pathID string, chunkIndex int, plainSize int64) []byte {
	return joinAAD("syna-blob-v1", workspaceID, rootID, pathID, strconv.Itoa(chunkIndex), strconv.FormatInt(plainSize, 10))
}

func EventAAD(workspaceID, rootID, pathID string, eventType string) []byte {
	return joinAAD("syna-event-v1", workspaceID, rootID, pathID, eventType)
}

func SnapshotAAD(workspaceID, rootID string, baseSeq int64) []byte {
	return joinAAD("syna-snapshot-v1", workspaceID, rootID, strconv.FormatInt(baseSeq, 10))
}

func joinAAD(parts ...string) []byte {
	buf := bytes.NewBuffer(nil)
	for i, part := range parts {
		if i > 0 {
			buf.WriteByte(0)
		}
		buf.WriteString(part)
	}
	return buf.Bytes()
}
