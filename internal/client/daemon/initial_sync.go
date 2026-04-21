package daemon

import (
	"context"

	"syna/internal/client/scanner"
	"syna/internal/client/snapshotter"
	"syna/internal/client/state"
	"syna/internal/client/uploader"
	commoncrypto "syna/internal/common/crypto"
	"syna/internal/common/protocol"
)

type initialRootSync struct {
	Entries    []state.Entry
	Snapshot   protocol.SnapshotPayload
	ObjectRefs []string
}

func (d *Daemon) submitInitialRootEntries(ctx context.Context, rootID, homeRelPath string, scan *scanner.Result, progress AddProgressFunc, progressState *addProgressState) (*initialRootSync, error) {
	sync := &initialRootSync{
		Snapshot: protocol.SnapshotPayload{
			RootID:      rootID,
			Kind:        scan.RootKind,
			HomeRelPath: homeRelPath,
		},
	}
	for _, item := range syncOrder(scan.Entries) {
		pathID := commoncrypto.PathID(d.keys, rootID, item.RelPath)
		if item.Kind == protocol.RootKindDir {
			if err := d.submitInitialDir(ctx, sync, rootID, pathID, item); err != nil {
				return nil, err
			}
			progressState.DoneEntries++
			reportAddProgress(progress, AddProgress{
				Stage:        "syncing",
				Message:      "synced directory",
				Path:         displaySyncPath(homeRelPath, item.RelPath),
				DoneBytes:    progressState.DoneBytes,
				TotalBytes:   progressState.TotalBytes,
				DoneFiles:    progressState.DoneFiles,
				TotalFiles:   progressState.TotalFiles,
				DoneEntries:  progressState.DoneEntries,
				TotalEntries: progressState.TotalEntries,
			})
			continue
		}
		if item.Kind == protocol.RootKindFile {
			if err := d.submitInitialFile(ctx, sync, rootID, pathID, item, homeRelPath, progress, progressState); err != nil {
				return nil, err
			}
		}
	}
	return sync, nil
}

func (d *Daemon) submitInitialDir(ctx context.Context, sync *initialRootSync, rootID, pathID string, item scanner.Entry) error {
	resp, err := d.submitEvent(ctx, rootID, pathID, "", protocol.EventDirPut, ptrInt64(0), protocol.DirPutPayload{
		Path:    item.RelPath,
		Mode:    item.Mode,
		MTimeNS: item.MTimeNS,
	}, nil)
	if err != nil {
		return err
	}
	sync.Entries = append(sync.Entries, state.Entry{
		RootID:     rootID,
		RelPath:    item.RelPath,
		PathID:     pathID,
		Kind:       protocol.RootKindDir,
		CurrentSeq: resp.AcceptedSeq,
		Mode:       item.Mode,
		MTimeNS:    item.MTimeNS,
	})
	sync.Snapshot.Entries = append(sync.Snapshot.Entries, protocol.SnapshotEntry{
		Path:    item.RelPath,
		Kind:    protocol.RootKindDir,
		Mode:    item.Mode,
		MTimeNS: item.MTimeNS,
	})
	return nil
}

func (d *Daemon) submitInitialFile(ctx context.Context, sync *initialRootSync, rootID, pathID string, item scanner.Entry, homeRelPath string, progress AddProgressFunc, progressState *addProgressState) error {
	progressPath := displaySyncPath(homeRelPath, item.RelPath)
	up, err := uploader.UploadFileWithProgress(ctx, d.conn, d.keys.BlobKey, d.cfg.WorkspaceID, rootID, pathID, item.RelPath, item.AbsPath, item.Mode, item.MTimeNS, func(upload uploader.Progress) {
		progressState.DoneBytes += upload.PlainBytes
		reportAddProgress(progress, AddProgress{
			Stage:        "syncing",
			Message:      "uploading file",
			Path:         progressPath,
			DoneBytes:    progressState.DoneBytes,
			TotalBytes:   progressState.TotalBytes,
			DoneFiles:    progressState.DoneFiles,
			TotalFiles:   progressState.TotalFiles,
			DoneEntries:  progressState.DoneEntries,
			TotalEntries: progressState.TotalEntries,
		})
	})
	if err != nil {
		return err
	}
	resp, err := d.submitEvent(ctx, rootID, pathID, "", protocol.EventFilePut, ptrInt64(0), up.Payload, up.Refs)
	if err != nil {
		return err
	}
	progressState.DoneFiles++
	progressState.DoneEntries++
	reportAddProgress(progress, AddProgress{
		Stage:        "syncing",
		Message:      "synced file",
		Path:         progressPath,
		DoneBytes:    progressState.DoneBytes,
		TotalBytes:   progressState.TotalBytes,
		DoneFiles:    progressState.DoneFiles,
		TotalFiles:   progressState.TotalFiles,
		DoneEntries:  progressState.DoneEntries,
		TotalEntries: progressState.TotalEntries,
	})
	sync.Entries = append(sync.Entries, state.Entry{
		RootID:        rootID,
		RelPath:       item.RelPath,
		PathID:        pathID,
		Kind:          protocol.RootKindFile,
		CurrentSeq:    resp.AcceptedSeq,
		ContentSHA256: up.Payload.ContentSHA256,
		SizeBytes:     up.Payload.SizeBytes,
		Mode:          up.Payload.Mode,
		MTimeNS:       up.Payload.MTimeNS,
	})
	sync.Snapshot.Entries = append(sync.Snapshot.Entries, protocol.SnapshotEntry{
		Path:          item.RelPath,
		Kind:          protocol.RootKindFile,
		Mode:          up.Payload.Mode,
		MTimeNS:       up.Payload.MTimeNS,
		SizeBytes:     up.Payload.SizeBytes,
		ContentSHA256: up.Payload.ContentSHA256,
		Chunks:        up.Payload.Chunks,
	})
	sync.Snapshot.BaseSeq = resp.AcceptedSeq
	sync.ObjectRefs = append(sync.ObjectRefs, up.Refs...)
	return nil
}

func (d *Daemon) publishInitialSnapshot(ctx context.Context, rootID string, sync *initialRootSync) {
	if len(sync.Entries) == 0 {
		return
	}
	lastSeq := sync.Entries[len(sync.Entries)-1].CurrentSeq
	sync.Snapshot.BaseSeq = lastSeq
	blob, objectID, err := snapshotter.BuildSnapshotBlob(d.keys, d.cfg.WorkspaceID, rootID, lastSeq, sync.Snapshot)
	if err != nil {
		return
	}
	if err := d.conn.UploadObject(ctx, objectID, "snapshot", int64(len(blob)), blob); err != nil {
		return
	}
	_, _ = d.conn.SubmitSnapshot(ctx, protocol.SnapshotSubmitRequest{
		RootID:     rootID,
		BaseSeq:    lastSeq,
		ObjectID:   objectID,
		ObjectRefs: dedupe(sync.ObjectRefs),
	})
}
