package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"syna/internal/client/agentrpc"
)

func TestConnectProgressRenderer(t *testing.T) {
	var output bytes.Buffer
	r := &addProgressRenderer{w: &output, enabled: true, started: time.Now()}
	fraction := 0.42
	r.Update(agentrpc.Progress{
		Stage: "downloading", Fraction: &fraction, Path: "notes/file.txt",
		DoneBytes: 500, TotalBytes: 1000, DoneFiles: 1, TotalFiles: 2,
	})
	for _, want := range []string{"Restoring", "[==========>-------------]", "~42%", "500 B/1.0 KB", "1/2 files", "notes/file.txt"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("progress missing %q: %q", want, output.String())
		}
	}
	// Stage changes must render even inside the normal redraw throttle.
	r.Update(agentrpc.Progress{Stage: "finalizing", Fraction: &fraction})
	if !strings.Contains(output.String(), "Finalizing") {
		t.Fatal("stage transition was throttled")
	}
	if strings.Contains(output.String(), "100%") {
		t.Fatal("premature completion")
	}
	r.Update(agentrpc.Progress{Stage: "done"})
	r.Done(nil)
	if !strings.HasSuffix(output.String(), "[========================] 100%\n") {
		t.Fatalf("missing completed bar: %q", output.String())
	}
}

func TestConnectProgressFractionReservesCompletion(t *testing.T) {
	for _, value := range []float64{0, 0.25, 0.999, 1, 2, -1} {
		fraction := progressFraction(agentrpc.Progress{Stage: "downloading", Fraction: &value})
		if fraction < 0 || fraction > 0.99 {
			t.Fatalf("fraction(%f) = %f, want 0..0.99", value, fraction)
		}
	}
	// Existing add progress still uses byte totals when no estimate is supplied.
	if fraction := progressFraction(agentrpc.Progress{DoneBytes: 5, TotalBytes: 10}); fraction != 0.5 {
		t.Fatalf("add fraction = %f", fraction)
	}
}

func TestConnectProgressRendererFailureAndRedirect(t *testing.T) {
	var output bytes.Buffer
	r := &addProgressRenderer{w: &output, enabled: true, started: time.Now()}
	r.Update(agentrpc.Progress{Stage: "connecting"})
	r.Done(errors.New("download failed"))
	if strings.Contains(output.String(), "100%") || !strings.HasSuffix(output.String(), "\r\x1b[2K") {
		t.Fatalf("failed progress was not cleared: %q", output.String())
	}
	file := t.TempDir() + "/stderr"
	w, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	redirected := newAddProgressRenderer(w)
	redirected.Update(agentrpc.Progress{Stage: "done"})
	redirected.Done(nil)
	info, err := w.Stat()
	if err != nil || info.Size() != 0 {
		t.Fatalf("redirected output contains animation: %v, %v", info, err)
	}
}
