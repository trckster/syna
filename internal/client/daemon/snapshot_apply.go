package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	var priorEntries map[string]state.Entry
	if !stageOnly {
		priorEntries, err = d.stateDB.EntriesForRoot(root.RootID)
		if err != nil {
			return err
		}
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
			dirSeq := baseSeq
			if prior, ok := priorEntries[relPath]; ok && !prior.Deleted && prior.Kind == protocol.RootKindDir && prior.Mode == entry.Mode && prior.MTimeNS == entry.MTimeNS {
				dirSeq = prior.CurrentSeq
			}
			entries = append(entries, state.Entry{
				RootID:     root.RootID,
				RelPath:    relPath,
				PathID:     pathID,
				Kind:       protocol.RootKindDir,
				CurrentSeq: dirSeq,
				Mode:       entry.Mode,
				MTimeNS:    entry.MTimeNS,
			})
		case protocol.RootKindFile:
			materialize := !stageOnly
			if materialize {
				localHash, localExists, err := fileSHA256Hex(target)
				if err != nil {
					return err
				}
				if localExists {
					prior, hasPrior := priorEntries[relPath]
					switch {
					case localHash == entry.ContentSHA256:
						// Local content already matches the snapshot; no
						// need to download or rewrite the file.
						materialize = false
					case hasPrior && !prior.Deleted && prior.ContentSHA256 == localHash:
						// Local file unchanged since last sync; the snapshot
						// is strictly newer, apply it normally.
					case hasPrior && !prior.Deleted && prior.ContentSHA256 == entry.ContentSHA256:
						// Snapshot matches our last synced state but the
						// local file was edited since: the local edit is
						// strictly newer. Keep it; the post-bootstrap rescan
						// will upload it.
						d.logger.Printf("snapshot %s: keeping newer local edit of %s", root.HomeRelPath, relPath)
						materialize = false
					default:
						// Both sides diverged (or we have no baseline).
						// Preserve the local version as a conflict copy
						// before applying the snapshot.
						conflictPath, err := d.preserveLocalConflictCopy(root, relPath, target)
						if err != nil {
							return err
						}
						d.logger.Printf("snapshot %s: local %s diverged from snapshot; preserved local copy as %s", root.HomeRelPath, relPath, conflictPath)
					}
				}
			}
			if materialize {
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
			fileSeq := baseSeq
			if prior, ok := priorEntries[relPath]; ok && !prior.Deleted && prior.Kind == protocol.RootKindFile && prior.ContentSHA256 == entry.ContentSHA256 {
				// The path is unchanged remotely since our last synced state;
				// the previously recorded per-path head seq is still correct,
				// while the snapshot's base seq would not match the server's
				// path head on the next submit.
				fileSeq = prior.CurrentSeq
			}
			entries = append(entries, state.Entry{
				RootID:        root.RootID,
				RelPath:       relPath,
				PathID:        pathID,
				Kind:          protocol.RootKindFile,
				CurrentSeq:    fileSeq,
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

func fileSHA256Hex(path string) (string, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() {
		return "", false, nil
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", false, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), true, nil
}

func (d *Daemon) preserveLocalConflictCopy(root state.Root, relPath, target string) (string, error) {
	name := conflictRelPath(relPath, root.TargetAbsPath, d.cfg.DeviceName, time.Now().UTC())
	dir := filepath.Dir(target)
	dst := filepath.Join(dir, filepath.Base(filepath.FromSlash(name)))
	src, err := os.Open(target)
	if err != nil {
		return "", err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(dir, ".syna-conflict-*")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return dst, nil
}

func (d *Daemon) validateChunkRef(chunk protocol.ChunkRef) error {
	if chunk.PlainSize <= 0 || chunk.PlainSize > uploader.ChunkSize {
		return &applier.IntegrityError{Message: "rejected remote chunk outside allowed size limits"}
	}
	return nil
}
