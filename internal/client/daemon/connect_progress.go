package daemon

import (
	"context"
	"fmt"
	"math"

	"syna/internal/common/protocol"
)

// Progress belongs to one connect request. Keeping it in the context prevents
// background sync and later retries from writing to a finished RPC stream.
type connectProgressKey struct{}

type connectProgress struct {
	emit       AddProgressFunc
	fraction   float64
	rootIndex  int
	rootCount  int
	eventBase  float64
	snapshot   AddProgress
	eventStart int64
	eventEnd   int64
	eventSeq   int64
	catchingUp bool
}

func connectProgressFrom(ctx context.Context) *connectProgress {
	progress, _ := ctx.Value(connectProgressKey{}).(*connectProgress)
	return progress
}

func (p *connectProgress) report(fraction float64, update AddProgress) {
	if p == nil || p.emit == nil {
		return
	}
	// New events or a required re-bootstrap can change the estimate. Never
	// move the displayed bar backwards, or report completion before success.
	fraction = math.Max(p.fraction, math.Max(0, math.Min(1, fraction)))
	if update.Stage != "done" {
		fraction = math.Min(fraction, 0.99)
	}
	p.fraction = fraction
	update.Fraction = &fraction
	p.emit(update)
}

func (p *connectProgress) startRoot(index, count int) {
	if p == nil {
		return
	}
	p.rootIndex, p.rootCount = index, count
	p.snapshot = AddProgress{Stage: "downloading", Message: fmt.Sprintf("restoring root %d/%d", index+1, count)}
	p.reportSnapshot()
}

func (p *connectProgress) planRoots(roots []protocol.BootstrapRoot) int {
	count := 0
	for _, root := range roots {
		if root.LatestSnapshotObjectID != "" && root.LatestSnapshotSeq > 0 {
			count++
		}
	}
	if p != nil {
		// Roots without snapshots are restored by replay. Allocate their
		// share to that phase instead of jumping to 85% before downloading.
		p.eventBase = 0.05 + 0.8*float64(count)/float64(max(1, len(roots)))
		p.catchingUp = false
	}
	return count
}

func (p *connectProgress) planSnapshot(snapshot protocol.SnapshotPayload) {
	if p == nil {
		return
	}
	p.snapshot.TotalEntries = len(snapshot.Entries)
	p.snapshot.Path = snapshot.HomeRelPath
	for _, entry := range snapshot.Entries {
		if entry.Kind == protocol.RootKindFile {
			p.snapshot.TotalFiles++
			p.snapshot.TotalBytes += entry.SizeBytes
		}
	}
	p.reportSnapshot()
}

func (p *connectProgress) snapshotBytes(n int64, path string) {
	if p == nil {
		return
	}
	p.snapshot.DoneBytes += n
	p.snapshot.Path = path
	p.reportSnapshot()
}

func (p *connectProgress) finishSnapshotEntry(entry protocol.SnapshotEntry, remainingBytes int64, path string) {
	if p == nil {
		return
	}
	p.snapshot.DoneBytes += remainingBytes
	p.snapshot.DoneEntries++
	if entry.Kind == protocol.RootKindFile {
		p.snapshot.DoneFiles++
	}
	p.snapshot.Path = path
	p.reportSnapshot()
}

func (p *connectProgress) reportSnapshot() {
	if p == nil || p.rootCount == 0 {
		return
	}
	var fraction float64
	if p.snapshot.TotalEntries > 0 {
		fraction = float64(p.snapshot.DoneEntries) / float64(p.snapshot.TotalEntries)
	}
	if p.snapshot.TotalBytes > 0 {
		// Include metadata work so empty files and directories also advance
		// the bar, and the last chunk alone cannot finish a root.
		fraction = 0.95*float64(p.snapshot.DoneBytes)/float64(p.snapshot.TotalBytes) + 0.05*fraction
	}
	// Metadata/authentication: 5%, restore/replay: 94%, finish: 1%.
	p.report(0.05+(p.eventBase-0.05)*(float64(p.rootIndex)+fraction)/float64(p.rootCount), p.snapshot)
}

func (p *connectProgress) startCatchUp(after, target int64) {
	if p == nil {
		return
	}
	if !p.catchingUp {
		p.eventStart = after
		p.catchingUp = true
	}
	p.eventEnd = target
	p.finishEvent(after)
}

func (p *connectProgress) startEvent(seq int64) {
	if p != nil {
		p.eventSeq = seq
	}
}

func (p *connectProgress) eventBytes(path string, done, total int64) {
	if p == nil || total <= 0 {
		return
	}
	fraction := float64(done) / float64(total)
	p.reportEvent(float64(p.eventSeq-1)+fraction, AddProgress{
		Stage: "catching_up", Path: path, DoneBytes: done, TotalBytes: total,
	})
}

func (p *connectProgress) finishEvent(seq int64) {
	if p == nil {
		return
	}
	p.reportEvent(float64(seq), AddProgress{
		Stage: "catching_up", Message: "applying remaining changes",
		DoneEntries: int(seq - p.eventStart), TotalEntries: int(p.eventEnd - p.eventStart),
	})
}

func (p *connectProgress) reportEvent(seq float64, update AddProgress) {
	fraction := 0.0
	if p.eventEnd > p.eventStart {
		fraction = (seq - float64(p.eventStart)) / float64(p.eventEnd-p.eventStart)
	}
	p.report(p.eventBase+(0.99-p.eventBase)*fraction, update)
}
