package snapshotter

import (
	"encoding/json"

	commoncrypto "syna/internal/common/crypto"
	"syna/internal/common/protocol"
)

func BuildSnapshotBlob(keys *commoncrypto.DerivedKeys, workspaceID, rootID string, baseSeq int64, payload protocol.SnapshotPayload) ([]byte, string, error) {
	plain, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	blob, err := commoncrypto.Encrypt(keys.SnapshotKey, plain, commoncrypto.SnapshotAAD(workspaceID, rootID, baseSeq))
	if err != nil {
		return nil, "", err
	}
	return blob, commoncrypto.ObjectID(blob), nil
}
