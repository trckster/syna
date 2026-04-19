package uploader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"

	"syna/internal/client/connector"
	commoncrypto "syna/internal/common/crypto"
	"syna/internal/common/protocol"
)

const ChunkSize = protocol.MaxFileChunkPlainSize

type Result struct {
	Payload protocol.FilePutPayload
	Refs    []string
}

func UploadFile(ctx context.Context, conn *connector.Client, blobKey []byte, workspaceID, rootID, pathID, relPath, absPath string, mode int64, mtimeNS int64) (*Result, error) {
	file, err := os.Open(absPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	h := sha256.New()
	var (
		offset int
		chunks []protocol.ChunkRef
		refs   []string
		total  int64
	)
	for {
		buf := make([]byte, ChunkSize)
		n, err := io.ReadFull(file, buf)
		if err == io.EOF {
			break
		}
		if err == io.ErrUnexpectedEOF {
			buf = buf[:n]
		} else if err != nil {
			return nil, err
		} else {
			buf = buf[:n]
		}
		if len(buf) == 0 {
			break
		}
		total += int64(len(buf))
		_, _ = h.Write(buf)
		blob, err := commoncrypto.Encrypt(blobKey, buf, commoncrypto.BlobAAD(workspaceID, rootID, pathID, offset, int64(len(buf))))
		if err != nil {
			return nil, err
		}
		objectID := commoncrypto.ObjectID(blob)
		if err := conn.UploadObject(ctx, objectID, "file_chunk", int64(len(buf)), blob); err != nil {
			return nil, err
		}
		chunks = append(chunks, protocol.ChunkRef{ObjectID: objectID, PlainSize: int64(len(buf))})
		refs = append(refs, objectID)
		offset++
		if n < ChunkSize {
			break
		}
	}
	return &Result{
		Payload: protocol.FilePutPayload{
			Path:          relPath,
			Mode:          mode,
			MTimeNS:       mtimeNS,
			SizeBytes:     total,
			ContentSHA256: hex.EncodeToString(h.Sum(nil)),
			Chunks:        chunks,
		},
		Refs: refs,
	}, nil
}
