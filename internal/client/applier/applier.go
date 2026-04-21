package applier

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"syna/internal/client/connector"
	"syna/internal/client/state"
	"syna/internal/client/uploader"
	commoncrypto "syna/internal/common/crypto"
	"syna/internal/common/paths"
	"syna/internal/common/protocol"
)

type ApplyOptions struct {
	StageOnly bool
}

type IntegrityError struct {
	Message string
}

func (e *IntegrityError) Error() string {
	return e.Message
}

func ApplyEvent(ctx context.Context, conn *connector.Client, keys *commoncrypto.DerivedKeys, workspaceID string, root state.Root, event protocol.EventRecord, stateDB *state.DB, opts ApplyOptions) error {
	pathID := ""
	if event.PathID != nil {
		pathID = *event.PathID
	}
	payloadBlob, err := commoncrypto.ParseBase64Raw(event.PayloadBlob)
	if err != nil {
		return err
	}
	plaintext, err := commoncrypto.Decrypt(keys.EventKey, payloadBlob, commoncrypto.EventAAD(workspaceID, event.RootID, pathID, string(event.EventType)))
	if err != nil {
		return err
	}
	switch event.EventType {
	case protocol.EventDirPut:
		var payload protocol.DirPutPayload
		if err := json.Unmarshal(plaintext, &payload); err != nil {
			return err
		}
		relPath, target, pathID, err := validateDirPutTarget(keys, root, payload.Path, event.PathID)
		if err != nil {
			return err
		}
		expected := state.Entry{
			RootID:  root.RootID,
			RelPath: relPath,
			Kind:    protocol.RootKindDir,
			Mode:    payload.Mode,
			MTimeNS: payload.MTimeNS,
			Deleted: false,
		}
		if !opts.StageOnly {
			if err := ensureSafeDirTarget(root, target); err != nil {
				return err
			}
			_ = os.Chmod(target, os.FileMode(payload.Mode))
			_ = os.Chtimes(target, time.Unix(0, payload.MTimeNS), time.Unix(0, payload.MTimeNS))
			if err := stateDB.SetIgnore(root.RootID, relPath, time.Now().UTC().Add(2*time.Second), expected); err != nil {
				return err
			}
		}
		return stateDB.UpsertEntry(state.Entry{
			RootID:     root.RootID,
			RelPath:    relPath,
			PathID:     pathID,
			Kind:       protocol.RootKindDir,
			CurrentSeq: event.Seq,
			Mode:       payload.Mode,
			MTimeNS:    payload.MTimeNS,
		})
	case protocol.EventFilePut:
		var payload protocol.FilePutPayload
		if err := json.Unmarshal(plaintext, &payload); err != nil {
			return err
		}
		relPath, target, pathID, err := validateFilePutTarget(keys, root, payload.Path, event.PathID)
		if err != nil {
			return err
		}
		expected := state.Entry{
			RootID:        root.RootID,
			RelPath:       relPath,
			Kind:          protocol.RootKindFile,
			ContentSHA256: payload.ContentSHA256,
			Mode:          payload.Mode,
			MTimeNS:       payload.MTimeNS,
			Deleted:       false,
		}
		if !opts.StageOnly {
			if err := ensureSafeParentDirs(root, target); err != nil {
				return err
			}
			tmp, err := os.CreateTemp(filepath.Dir(target), ".syna-*")
			if err != nil {
				return err
			}
			hasher := sha256.New()
			var total int64
			for i, chunk := range payload.Chunks {
				if err := validateChunkRef(chunk); err != nil {
					tmp.Close()
					_ = os.Remove(tmp.Name())
					return err
				}
				maxBlobSize, err := commoncrypto.EncryptedSize(chunk.PlainSize)
				if err != nil {
					tmp.Close()
					_ = os.Remove(tmp.Name())
					return err
				}
				n, err := DownloadAndDecryptObjectTo(ctx, conn, chunk.ObjectID, maxBlobSize, keys.BlobKey, commoncrypto.BlobAAD(workspaceID, root.RootID, pathID, i, chunk.PlainSize), io.MultiWriter(tmp, hasher))
				if err != nil {
					tmp.Close()
					_ = os.Remove(tmp.Name())
					return err
				}
				if n != chunk.PlainSize {
					tmp.Close()
					_ = os.Remove(tmp.Name())
					return &IntegrityError{Message: "rejected remote file_put with inconsistent chunk size"}
				}
				total += n
			}
			if total != payload.SizeBytes {
				tmp.Close()
				_ = os.Remove(tmp.Name())
				return &IntegrityError{Message: "rejected remote file_put with inconsistent size metadata"}
			}
			if got := hex.EncodeToString(hasher.Sum(nil)); got != payload.ContentSHA256 {
				tmp.Close()
				_ = os.Remove(tmp.Name())
				return &IntegrityError{Message: "rejected remote file_put with inconsistent content digest"}
			}
			if err := tmp.Sync(); err != nil {
				tmp.Close()
				_ = os.Remove(tmp.Name())
				return err
			}
			if err := tmp.Close(); err != nil {
				_ = os.Remove(tmp.Name())
				return err
			}
			if err := rejectSymlinkParents(root, target); err != nil {
				_ = os.Remove(tmp.Name())
				return err
			}
			if err := os.Rename(tmp.Name(), target); err != nil {
				_ = os.Remove(tmp.Name())
				return err
			}
			_ = os.Chmod(target, os.FileMode(payload.Mode))
			_ = os.Chtimes(target, time.Unix(0, payload.MTimeNS), time.Unix(0, payload.MTimeNS))
			if err := stateDB.SetIgnore(root.RootID, relPath, time.Now().UTC().Add(2*time.Second), expected); err != nil {
				return err
			}
		}
		return stateDB.UpsertEntry(state.Entry{
			RootID:        root.RootID,
			RelPath:       relPath,
			PathID:        pathID,
			Kind:          protocol.RootKindFile,
			CurrentSeq:    event.Seq,
			ContentSHA256: payload.ContentSHA256,
			SizeBytes:     payload.SizeBytes,
			Mode:          payload.Mode,
			MTimeNS:       payload.MTimeNS,
		})
	case protocol.EventDelete:
		var payload protocol.DeletePayload
		if err := json.Unmarshal(plaintext, &payload); err != nil {
			return err
		}
		relPath, target, pathID, err := resolveDeleteTarget(root, payload.Path, event.PathID, keys)
		if err != nil {
			return err
		}
		if !opts.StageOnly {
			if err := rejectSymlinkParents(root, target); err != nil {
				return err
			}
			_ = os.RemoveAll(target)
			if err := stateDB.SetIgnoreDelete(root.RootID, relPath, time.Now().UTC().Add(2*time.Second)); err != nil {
				return err
			}
		}
		return stateDB.MarkEntryDeleted(state.Entry{
			RootID:     root.RootID,
			RelPath:    relPath,
			PathID:     pathID,
			CurrentSeq: event.Seq,
		})
	default:
		return fmt.Errorf("unsupported event type %s", event.EventType)
	}
}

func validateDirPutTarget(keys *commoncrypto.DerivedKeys, root state.Root, rawPath string, eventPathID *string) (string, string, string, error) {
	relPath, target, pathID, err := resolveContentTarget(keys, root, rawPath, true, eventPathID)
	if err != nil {
		return "", "", "", err
	}
	if relPath == "" && root.Kind != protocol.RootKindDir {
		return "", "", "", &IntegrityError{Message: "rejected remote dir_put for non-directory root"}
	}
	return relPath, target, pathID, nil
}

func validateFilePutTarget(keys *commoncrypto.DerivedKeys, root state.Root, rawPath string, eventPathID *string) (string, string, string, error) {
	relPath, target, pathID, err := resolveContentTarget(keys, root, rawPath, true, eventPathID)
	if err != nil {
		return "", "", "", err
	}
	if relPath == "" && root.Kind != protocol.RootKindFile {
		return "", "", "", &IntegrityError{Message: "rejected remote file_put for directory root"}
	}
	return relPath, target, pathID, nil
}

func resolveDeleteTarget(root state.Root, rawPath string, eventPathID *string, keys *commoncrypto.DerivedKeys) (string, string, string, error) {
	relPath, target, pathID, err := resolveContentTarget(keys, root, rawPath, false, eventPathID)
	return relPath, target, pathID, err
}

func resolveContentTarget(keys *commoncrypto.DerivedKeys, root state.Root, rawPath string, allowRoot bool, eventPathID *string) (string, string, string, error) {
	if eventPathID == nil || *eventPathID == "" {
		return "", "", "", &IntegrityError{Message: "rejected remote event with missing path_id"}
	}
	var (
		relPath string
		target  string
		err     error
	)
	switch root.Kind {
	case protocol.RootKindDir:
		relPath, target, err = paths.ResolveRemoteTarget(root.TargetAbsPath, rawPath, allowRoot)
		if err != nil {
			return "", "", "", &IntegrityError{Message: "rejected remote event with invalid path"}
		}
	case protocol.RootKindFile:
		if rawPath != "" || !allowRoot {
			return "", "", "", &IntegrityError{Message: "rejected remote event path outside file root"}
		}
		relPath = ""
		target = filepath.Clean(root.TargetAbsPath)
	default:
		return "", "", "", fmt.Errorf("unsupported root kind %s", root.Kind)
	}
	pathID := commoncrypto.PathID(keys, root.RootID, relPath)
	if pathID != *eventPathID {
		return "", "", "", &IntegrityError{Message: "rejected remote event with mismatched path_id"}
	}
	return relPath, target, pathID, nil
}

func ensureSafeDirTarget(root state.Root, target string) error {
	switch root.Kind {
	case protocol.RootKindDir:
		return paths.EnsureSafeDir(root.TargetAbsPath, target, 0o755)
	case protocol.RootKindFile:
		return fmt.Errorf("cannot materialize directory entry inside file root")
	default:
		return fmt.Errorf("unsupported root kind %s", root.Kind)
	}
}

func ensureSafeParentDirs(root state.Root, target string) error {
	switch root.Kind {
	case protocol.RootKindDir:
		return paths.EnsureSafeParents(root.TargetAbsPath, target, 0o755)
	case protocol.RootKindFile:
		return paths.EnsureSafeDir(filepath.Dir(root.TargetAbsPath), filepath.Dir(target), 0o755)
	default:
		return fmt.Errorf("unsupported root kind %s", root.Kind)
	}
}

func rejectSymlinkParents(root state.Root, target string) error {
	switch root.Kind {
	case protocol.RootKindDir:
		return paths.RejectSymlinkParents(root.TargetAbsPath, target)
	case protocol.RootKindFile:
		return paths.RejectSymlinkParents(filepath.Dir(root.TargetAbsPath), target)
	default:
		return fmt.Errorf("unsupported root kind %s", root.Kind)
	}
}

func validateChunkRef(chunk protocol.ChunkRef) error {
	if chunk.PlainSize <= 0 || chunk.PlainSize > uploader.ChunkSize {
		return &IntegrityError{Message: "rejected remote file chunk outside allowed size limits"}
	}
	return nil
}

func DownloadAndDecryptObjectTo(ctx context.Context, conn *connector.Client, objectID string, maxBlobSize int64, key, aad []byte, dst io.Writer) (int64, error) {
	encrypted := bytes.NewBuffer(nil)
	if _, err := conn.DownloadObjectTo(ctx, objectID, maxBlobSize, encrypted); err != nil {
		return 0, err
	}
	return commoncrypto.DecryptToWriter(key, encrypted.Bytes(), aad, dst)
}
