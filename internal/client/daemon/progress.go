package daemon

import (
	"path"

	"syna/internal/client/scanner"
	"syna/internal/common/protocol"
)

type AddProgress struct {
	Stage        string
	Message      string
	Path         string
	DoneBytes    int64
	TotalBytes   int64
	DoneFiles    int
	TotalFiles   int
	DoneEntries  int
	TotalEntries int
}

type AddProgressFunc func(AddProgress)

type addProgressState struct {
	DoneBytes    int64
	TotalBytes   int64
	DoneFiles    int
	TotalFiles   int
	DoneEntries  int
	TotalEntries int
}

func addProgressTotals(scan *scanner.Result) *addProgressState {
	totals := &addProgressState{
		TotalEntries: len(scan.Entries),
	}
	for _, entry := range scan.Entries {
		if entry.Kind != protocol.RootKindFile {
			continue
		}
		totals.TotalFiles++
		totals.TotalBytes += entry.SizeBytes
	}
	return totals
}

func reportAddProgress(progress AddProgressFunc, update AddProgress) {
	if progress != nil {
		progress(update)
	}
}

func displaySyncPath(homeRelPath, relPath string) string {
	if relPath == "" {
		return homeRelPath
	}
	return path.Join(homeRelPath, relPath)
}
