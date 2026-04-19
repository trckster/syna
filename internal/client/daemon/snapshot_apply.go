package daemon

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

	"syna/internal/client/applier"
	"syna/internal/client/state"
	"syna/internal/client/uploader"
	commoncrypto "syna/internal/common/crypto"
	"syna/internal/common/paths"
	"syna/internal/common/protocol"
)

func (d *Daemon) applySnapshot(ctx context.Context, root state.Root, objectID string, baseSeq int64, stageOnly bool) error {
	maxSnapshotBlobSize, err := commoncrypto.EncryptedSize(protocol.MaxSnapshotPlainSize)
	if err != nil {
		return err
	}
	encryptedSnapshot := bytes.NewBuffer(nil)
	if _, err := d.conn.DownloadObjectTo(ctx, objectID, maxSnapshotBlobSize, encryptedSnapshot); err != nil {
		return err
	}
	plain, err := commoncrypto.DecryptInPlace(d.keys.SnapshotKey, encryptedSnapshot.Bytes(), commoncrypto.SnapshotAAD(d.cfg.WorkspaceID, root.RootID, baseSeq))
	if err != nil {
		return err
	}
	var snapshot protocol.SnapshotPayload
	if err := json.Unmarshal(plain, &snapshot); err != nil {
		return err
	}
	if snapshot.RootID != root.RootID {
		return &applier.IntegrityError{Message: "rejected snapshot with mismatched root_id"}
	}
	if snapshot.BaseSeq != baseSeq {
		return &applier.IntegrityError{Message: "rejected snapshot with mismatched base_seq"}
	}
	if snapshot.Kind != root.Kind {
		return &applier.IntegrityError{Message: "rejected snapshot with mismatched root kind"}
	}
	homeRel, _, err := paths.ResolveHomeRelTarget(snapshot.HomeRelPath)
	if err != nil {
		return &applier.IntegrityError{Message: "rejected snapshot with invalid home_rel_path"}
	}
	if homeRel != root.HomeRelPath || commoncrypto.RootID(d.keys, homeRel) != root.RootID {
		return &applier.IntegrityError{Message: "rejected snapshot with invalid root binding"}
	}
	var entries []state.Entry
	for _, entry := range snapshot.Entries {
		relPath, target, pathID, err := d.resolveSnapshotTarget(root, entry.Path, entry.Kind)
		if err != nil {
			return err
		}
		switch entry.Kind {
		case protocol.RootKindDir:
			if !stageOnly {
				if err := d.ensureSafeDirTarget(root, target); err != nil {
					return err
				}
				_ = os.Chmod(target, os.FileMode(entry.Mode))
				_ = os.Chtimes(target, time.Unix(0, entry.MTimeNS), time.Unix(0, entry.MTimeNS))
			}
			entries = append(entries, state.Entry{
				RootID:     root.RootID,
				RelPath:    relPath,
				PathID:     pathID,
				Kind:       protocol.RootKindDir,
				CurrentSeq: baseSeq,
				Mode:       entry.Mode,
				MTimeNS:    entry.MTimeNS,
			})
		case protocol.RootKindFile:
			if !stageOnly {
				if err := d.ensureSafeParentDirs(root, target); err != nil {
					return err
				}
				tmp, err := os.CreateTemp(filepath.Dir(target), ".syna-bootstrap-*")
				if err != nil {
					return err
				}
				hasher := sha256.New()
				var total int64
				for i, chunk := range entry.Chunks {
					if err := d.validateChunkRef(chunk); err != nil {
						tmp.Close()
						_ = os.Remove(tmp.Name())
						return err
					}
					maxChunkBlobSize, err := commoncrypto.EncryptedSize(chunk.PlainSize)
					if err != nil {
						tmp.Close()
						_ = os.Remove(tmp.Name())
						return err
					}
					n, err := applier.DownloadAndDecryptObjectTo(ctx, d.conn, chunk.ObjectID, maxChunkBlobSize, d.keys.BlobKey, commoncrypto.BlobAAD(d.cfg.WorkspaceID, root.RootID, pathID, i, chunk.PlainSize), io.MultiWriter(tmp, hasher))
					if err != nil {
						tmp.Close()
						_ = os.Remove(tmp.Name())
						return err
					}
					if n != chunk.PlainSize {
						tmp.Close()
						_ = os.Remove(tmp.Name())
						return &applier.IntegrityError{Message: "rejected snapshot with inconsistent chunk size"}
					}
					total += n
				}
				if total != entry.SizeBytes {
					tmp.Close()
					_ = os.Remove(tmp.Name())
					return &applier.IntegrityError{Message: "rejected snapshot with inconsistent size metadata"}
				}
				if got := hex.EncodeToString(hasher.Sum(nil)); got != entry.ContentSHA256 {
					tmp.Close()
					_ = os.Remove(tmp.Name())
					return &applier.IntegrityError{Message: "rejected snapshot with inconsistent content digest"}
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
				if err := d.rejectSymlinkParents(root, target); err != nil {
					_ = os.Remove(tmp.Name())
					return err
				}
				if err := os.Rename(tmp.Name(), target); err != nil {
					_ = os.Remove(tmp.Name())
					return err
				}
				_ = os.Chmod(target, os.FileMode(entry.Mode))
				_ = os.Chtimes(target, time.Unix(0, entry.MTimeNS), time.Unix(0, entry.MTimeNS))
			}
			entries = append(entries, state.Entry{
				RootID:        root.RootID,
				RelPath:       relPath,
				PathID:        pathID,
				Kind:          protocol.RootKindFile,
				CurrentSeq:    baseSeq,
				ContentSHA256: entry.ContentSHA256,
				SizeBytes:     entry.SizeBytes,
				Mode:          entry.Mode,
				MTimeNS:       entry.MTimeNS,
			})
		}
	}
	return d.stateDB.ReplaceEntries(root.RootID, entries)
}

func (d *Daemon) resolveSnapshotTarget(root state.Root, rawPath string, kind protocol.RootKind) (string, string, string, error) {
	switch root.Kind {
	case protocol.RootKindDir:
		relPath, target, err := paths.ResolveRemoteTarget(root.TargetAbsPath, rawPath, true)
		if err != nil {
			return "", "", "", &applier.IntegrityError{Message: "rejected snapshot entry with invalid path"}
		}
		if relPath == "" && kind != protocol.RootKindDir {
			return "", "", "", &applier.IntegrityError{Message: "rejected snapshot root entry with invalid kind"}
		}
		return relPath, target, commoncrypto.PathID(d.keys, root.RootID, relPath), nil
	case protocol.RootKindFile:
		if rawPath != "" || kind != protocol.RootKindFile {
			return "", "", "", &applier.IntegrityError{Message: "rejected snapshot entry outside file root"}
		}
		return "", filepath.Clean(root.TargetAbsPath), commoncrypto.PathID(d.keys, root.RootID, ""), nil
	default:
		return "", "", "", fmt.Errorf("unsupported root kind %s", root.Kind)
	}
}

func (d *Daemon) ensureSafeDirTarget(root state.Root, target string) error {
	switch root.Kind {
	case protocol.RootKindDir:
		return paths.EnsureSafeDir(root.TargetAbsPath, target, 0o755)
	case protocol.RootKindFile:
		return fmt.Errorf("cannot materialize directory entry inside file root")
	default:
		return fmt.Errorf("unsupported root kind %s", root.Kind)
	}
}

func (d *Daemon) ensureSafeParentDirs(root state.Root, target string) error {
	switch root.Kind {
	case protocol.RootKindDir:
		return paths.EnsureSafeParents(root.TargetAbsPath, target, 0o755)
	case protocol.RootKindFile:
		return paths.EnsureSafeDir(filepath.Dir(root.TargetAbsPath), filepath.Dir(target), 0o755)
	default:
		return fmt.Errorf("unsupported root kind %s", root.Kind)
	}
}

func (d *Daemon) rejectSymlinkParents(root state.Root, target string) error {
	switch root.Kind {
	case protocol.RootKindDir:
		return paths.RejectSymlinkParents(root.TargetAbsPath, target)
	case protocol.RootKindFile:
		return paths.RejectSymlinkParents(filepath.Dir(root.TargetAbsPath), target)
	default:
		return fmt.Errorf("unsupported root kind %s", root.Kind)
	}
}

func (d *Daemon) validateChunkRef(chunk protocol.ChunkRef) error {
	if chunk.PlainSize <= 0 || chunk.PlainSize > uploader.ChunkSize {
		return &applier.IntegrityError{Message: "rejected remote chunk outside allowed size limits"}
	}
	return nil
}
