package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"syna/internal/client/agentrpc"
)

type addProgressRenderer struct {
	w        io.Writer
	enabled  bool
	started  time.Time
	frame    int
	lastDraw time.Time
	drew     bool
	done     bool
}

func newAddProgressRenderer(w *os.File) *addProgressRenderer {
	return &addProgressRenderer{
		w:       w,
		enabled: isTerminal(w),
		started: time.Now(),
	}
}

func (r *addProgressRenderer) Update(progress agentrpc.Progress) {
	if !r.enabled {
		return
	}
	now := time.Now()
	if !r.shouldDraw(progress, now) {
		return
	}
	r.draw(progress, now)
}

func (r *addProgressRenderer) Done(err error) {
	if !r.enabled || r.done {
		return
	}
	if err != nil {
		if r.drew {
			fmt.Fprint(r.w, "\r\x1b[2K")
		}
		return
	}
	if r.drew {
		fmt.Fprint(r.w, "\n")
	}
}

func (r *addProgressRenderer) shouldDraw(progress agentrpc.Progress, now time.Time) bool {
	if !r.drew || progress.Stage == "done" || progress.Stage == "finalizing" || progress.Stage == "scanning" {
		return true
	}
	return now.Sub(r.lastDraw) >= 100*time.Millisecond
}

func (r *addProgressRenderer) draw(progress agentrpc.Progress, now time.Time) {
	frames := `-\|/`
	spinner := frames[r.frame%len(frames)]
	r.frame++

	fraction := progressFraction(progress)
	bar := progressBar(fraction, 24)
	percent := int(math.Round(fraction * 100))
	title := progressTitle(progress.Stage)

	line := fmt.Sprintf("%s %c [%s] %3d%%", title, spinner, bar, percent)
	if progress.TotalBytes > 0 {
		line += fmt.Sprintf("  %s/%s", formatBytes(progress.DoneBytes), formatBytes(progress.TotalBytes))
	}
	if progress.TotalFiles > 0 {
		line += fmt.Sprintf("  %d/%d files", progress.DoneFiles, progress.TotalFiles)
	} else if progress.TotalEntries > 0 {
		line += fmt.Sprintf("  %d/%d entries", progress.DoneEntries, progress.TotalEntries)
	}
	if eta := progressETA(progress, now.Sub(r.started)); eta != "" {
		line += "  ETA " + eta
	}
	if progress.Path != "" {
		line += "  " + shortenMiddle(progress.Path, 44)
	} else if progress.Message != "" {
		line += "  " + progress.Message
	}

	fmt.Fprintf(r.w, "\r\x1b[2K%s", line)
	r.lastDraw = now
	r.drew = true
	if progress.Stage == "done" {
		r.done = true
		fmt.Fprint(r.w, "\n")
	}
}

func progressTitle(stage string) string {
	switch stage {
	case "scanning":
		return "Scanning "
	case "finalizing":
		return "Finalizing"
	case "done":
		return "Synced   "
	default:
		return "Syncing  "
	}
}

func progressFraction(progress agentrpc.Progress) float64 {
	if progress.Stage == "done" {
		return 1
	}
	var fraction float64
	if progress.TotalBytes > 0 {
		fraction = float64(progress.DoneBytes) / float64(progress.TotalBytes)
	} else if progress.TotalEntries > 0 {
		fraction = float64(progress.DoneEntries) / float64(progress.TotalEntries)
	}
	if fraction < 0 {
		return 0
	}
	if fraction > 1 {
		return 1
	}
	return fraction
}

func progressBar(fraction float64, width int) string {
	if width <= 0 {
		return ""
	}
	if fraction >= 1 {
		return strings.Repeat("=", width)
	}
	filled := int(fraction * float64(width))
	if filled >= width {
		filled = width - 1
	}
	return strings.Repeat("=", filled) + ">" + strings.Repeat("-", width-filled-1)
}

func progressETA(progress agentrpc.Progress, elapsed time.Duration) string {
	if progress.Stage == "scanning" || progress.Stage == "done" {
		return ""
	}
	fraction := progressFraction(progress)
	if fraction <= 0 {
		return "--"
	}
	if fraction >= 1 {
		return ""
	}
	remaining := time.Duration(float64(elapsed) * (1 - fraction) / fraction)
	return formatDuration(remaining)
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	seconds := int(d.Round(time.Second).Seconds())
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	seconds %= 60
	if minutes < 60 {
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	}
	hours := minutes / 60
	minutes %= 60
	return fmt.Sprintf("%dh%02dm%02ds", hours, minutes, seconds)
}

func formatBytes(bytes int64) string {
	const unit = 1000
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	for _, suffix := range []string{"KB", "MB", "GB", "TB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PB", value/unit)
}

func shortenMiddle(value string, max int) string {
	if len(value) <= max || max < 8 {
		return value
	}
	keep := max - 3
	left := keep / 2
	right := keep - left
	return value[:left] + "..." + value[len(value)-right:]
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
