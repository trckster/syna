package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"syna/internal/client/scanner"
	"syna/internal/client/state"
	"syna/internal/client/uploader"
	commoncrypto "syna/internal/common/crypto"
	"syna/internal/common/paths"
	"syna/internal/common/protocol"
)

func (d *Daemon) resolveFileConflict(ctx context.Context, root state.Root, item scanner.Entry, conflict *PathConflictError) error {
	stagedPath, cleanup, err := d.stageLocalFileCopy(item.AbsPath)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := d.applyCurrentRemoteHead(ctx, conflict.CurrentSeq); err != nil {
		return err
	}
	retryOnly, matches, err := d.remoteHeadAllowsRetry(root.RootID, item)
	if err != nil {
		return err
	}
	if retryOnly || matches {
		return nil
	}
	for attempt := 0; attempt < 5; attempt++ {
		conflictTime := time.Now().UTC().Add(time.Duration(attempt) * time.Second)
		conflictRelPath, conflictAbsPath, syncable, err := d.writeConflictCopy(root, item, stagedPath, conflictTime)
		if err != nil {
			return err
		}
		if !syncable {
			return nil
		}
		conflictPathID := commoncrypto.PathID(d.keys, root.RootID, conflictRelPath)
		baseSeq := int64(0)
		entries, err := d.stateDB.EntriesForRoot(root.RootID)
		if err != nil {
			return err
		}
		if existing, ok := entries[conflictRelPath]; ok {
			baseSeq = existing.CurrentSeq
		}
		up, err := uploader.UploadFile(ctx, d.conn, d.keys.BlobKey, d.cfg.WorkspaceID, root.RootID, conflictPathID, conflictRelPath, conflictAbsPath, item.Mode, item.MTimeNS)
		if err != nil {
			return err
		}
		resp, err := d.submitEvent(ctx, root.RootID, conflictPathID, "", protocol.EventFilePut, &baseSeq, up.Payload, up.Refs)
		if err == nil {
			return d.stateDB.UpsertEntry(state.Entry{
				RootID:        root.RootID,
				RelPath:       conflictRelPath,
				PathID:        conflictPathID,
				Kind:          protocol.RootKindFile,
				CurrentSeq:    resp.AcceptedSeq,
				ContentSHA256: up.Payload.ContentSHA256,
				SizeBytes:     up.Payload.SizeBytes,
				Mode:          up.Payload.Mode,
				MTimeNS:       up.Payload.MTimeNS,
			})
		}
		var nextConflict *PathConflictError
		if !errors.As(err, &nextConflict) {
			return err
		}
		_ = os.Remove(conflictAbsPath)
		if err := d.applyCurrentRemoteHead(ctx, nextConflict.CurrentSeq); err != nil {
			return err
		}
	}
	return fmt.Errorf("conflict copy upload retries exhausted")
}

func (d *Daemon) remoteHeadAllowsRetry(rootID string, item scanner.Entry) (bool, bool, error) {
	entries, err := d.stateDB.EntriesForRoot(rootID)
	if err != nil {
		return false, false, err
	}
	current, ok := entries[item.RelPath]
	if !ok || current.Deleted {
		return true, false, nil
	}
	if current.Kind != protocol.RootKindFile {
		return false, false, nil
	}
	return false, current.ContentSHA256 == item.ContentSHA256, nil
}

func (d *Daemon) stageLocalFileCopy(sourcePath string) (string, func(), error) {
	src, err := os.Open(sourcePath)
	if err != nil {
		return "", nil, err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(d.paths.StateDir, "syna-conflict-*")
	if err != nil {
		return "", nil, err
	}
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", nil, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", nil, err
	}
	return tmp.Name(), func() { _ = os.Remove(tmp.Name()) }, nil
}

func (d *Daemon) writeConflictCopy(root state.Root, item scanner.Entry, stagedPath string, now time.Time) (string, string, bool, error) {
	relPath := conflictRelPath(item.RelPath, root.TargetAbsPath, d.cfg.DeviceName, now)
	targetDir := root.TargetAbsPath
	containmentRoot := root.TargetAbsPath
	syncable := root.Kind == protocol.RootKindDir
	if syncable {
		dirHint := filepath.ToSlash(filepath.Dir(filepath.FromSlash(relPath)))
		if dirHint == "." {
			dirHint = ""
		}
		var err error
		_, targetDir, err = paths.ResolveRemoteTarget(root.TargetAbsPath, dirHint, true)
		if err != nil {
			return "", "", false, err
		}
	} else {
		relPath = filepath.Base(relPath)
		targetDir = filepath.Dir(root.TargetAbsPath)
		containmentRoot = targetDir
	}
	if err := paths.EnsureSafeDir(containmentRoot, targetDir, 0o755); err != nil {
		return "", "", false, err
	}
	finalPath := filepath.Join(targetDir, filepath.Base(filepath.FromSlash(relPath)))
	src, err := os.Open(stagedPath)
	if err != nil {
		return "", "", false, err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(targetDir, ".syna-conflict-*")
	if err != nil {
		return "", "", false, err
	}
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", "", false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", "", false, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", "", false, err
	}
	if err := paths.RejectSymlinkParents(containmentRoot, finalPath); err != nil {
		_ = os.Remove(tmp.Name())
		return "", "", false, err
	}
	if err := os.Rename(tmp.Name(), finalPath); err != nil {
		_ = os.Remove(tmp.Name())
		return "", "", false, err
	}
	if err := paths.RejectSymlinkParents(containmentRoot, finalPath); err != nil {
		return "", "", false, err
	}
	_ = os.Chmod(finalPath, os.FileMode(item.Mode))
	if err := paths.RejectSymlinkParents(containmentRoot, finalPath); err != nil {
		return "", "", false, err
	}
	_ = os.Chtimes(finalPath, time.Unix(0, item.MTimeNS), time.Unix(0, item.MTimeNS))
	if syncable {
		if err := d.stateDB.SetIgnore(root.RootID, relPath, time.Now().UTC().Add(2*time.Second), state.Entry{
			RootID:        root.RootID,
			RelPath:       relPath,
			Kind:          protocol.RootKindFile,
			ContentSHA256: item.ContentSHA256,
			Mode:          item.Mode,
			MTimeNS:       item.MTimeNS,
		}); err != nil {
			return "", "", false, err
		}
	}
	return filepath.ToSlash(relPath), finalPath, syncable, nil
}

func conflictRelPath(relPath, rootTargetAbsPath, deviceName string, now time.Time) string {
	basePath := relPath
	if basePath == "" {
		basePath = filepath.Base(rootTargetAbsPath)
	}
	dir := filepath.Dir(basePath)
	name := filepath.Base(basePath)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	conflictName := fmt.Sprintf("%s.syna-conflict-%s-%s%s", stem, deviceShort(deviceName), now.UTC().Format("20060102T150405Z"), ext)
	if dir == "." || dir == "" {
		return conflictName
	}
	return filepath.ToSlash(filepath.Join(dir, conflictName))
}

func deviceShort(deviceName string) string {
	slug := strings.ToLower(deviceName)
	re := regexp.MustCompile(`[^a-z0-9]+`)
	slug = re.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "device"
	}
	if len(slug) > 16 {
		slug = slug[:16]
	}
	return slug
}
