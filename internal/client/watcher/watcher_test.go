package watcher

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagerReportsCommonFileEvents(t *testing.T) {
	root := t.TempDir()
	changes := make(chan Change, 32)
	m, err := New(func(change Change) {
		changes <- change
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()
	if err := m.AddRoot("root-1", root); err != nil {
		t.Fatalf("AddRoot: %v", err)
	}

	mustWriteWatcherFile(t, filepath.Join(root, "created.txt"), "created")
	waitForChange(t, changes, "created.txt")

	mustWriteWatcherFile(t, filepath.Join(root, "created.txt"), "edited")
	waitForChange(t, changes, "created.txt")

	if err := os.Remove(filepath.Join(root, "created.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	waitForChange(t, changes, "created.txt")

	if err := os.Mkdir(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatalf("Mkdir(nested): %v", err)
	}
	waitForChange(t, changes, "nested")

	mustWriteWatcherFile(t, filepath.Join(root, "nested", "rename-source.txt"), "rename")
	waitForChange(t, changes, "nested/rename-source.txt")
	if err := os.Rename(filepath.Join(root, "nested", "rename-source.txt"), filepath.Join(root, "nested", "rename-target.txt")); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	waitForAnyChange(t, changes, map[string]bool{
		"nested/rename-source.txt": true,
		"nested/rename-target.txt": true,
		"nested":                   true,
	})

	if err := os.Chmod(filepath.Join(root, "nested", "rename-target.txt"), 0o600); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	waitForChange(t, changes, "nested/rename-target.txt")

	mustWriteWatcherFile(t, filepath.Join(root, ".atomic.tmp"), "atomic")
	waitForChange(t, changes, ".atomic.tmp")
	if err := os.Rename(filepath.Join(root, ".atomic.tmp"), filepath.Join(root, "atomic.txt")); err != nil {
		t.Fatalf("Rename(atomic): %v", err)
	}
	waitForAnyChange(t, changes, map[string]bool{
		".atomic.tmp": true,
		"atomic.txt":  true,
	})
}

func TestManagerSupportsFileRootsSharingParent(t *testing.T) {
	parent := t.TempDir()
	first := filepath.Join(parent, "first.txt")
	second := filepath.Join(parent, "second.txt")
	mustWriteWatcherFile(t, first, "first")
	mustWriteWatcherFile(t, second, "second")

	changes := make(chan Change, 32)
	m, err := New(func(change Change) {
		changes <- change
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()
	if err := m.AddRoot("root-first", first); err != nil {
		t.Fatalf("AddRoot(first): %v", err)
	}
	if err := m.AddRoot("root-second", second); err != nil {
		t.Fatalf("AddRoot(second): %v", err)
	}

	mustWriteWatcherFile(t, first, "first updated")
	waitForRootChange(t, changes, "root-first", "")

	m.RemoveRoot("root-first")
	mustWriteWatcherFile(t, second, "second updated")
	waitForRootChange(t, changes, "root-second", "")
}

func TestManagerIgnoresInternalStagingFilesButReportsRenameTarget(t *testing.T) {
	root := t.TempDir()
	changes := make(chan Change, 32)
	m, err := New(func(change Change) {
		changes <- change
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if err := m.AddRoot("root-1", root); err != nil {
		t.Fatalf("AddRoot: %v", err)
	}

	staging := filepath.Join(root, ".syna-download-1")
	mustWriteWatcherFile(t, staging, "remote content")
	assertNoWatcherChange(t, changes, 250*time.Millisecond)

	if err := os.Rename(staging, filepath.Join(root, "final.txt")); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	waitForChange(t, changes, "final.txt")
}

func TestManagerReportsChangesForPrefixedFileRoot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, ".syna-user-file")
	mustWriteWatcherFile(t, root, "initial")
	changes := make(chan Change, 8)
	m, err := New(func(change Change) { changes <- change })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if err := m.AddRoot("root-prefixed", root); err != nil {
		t.Fatalf("AddRoot: %v", err)
	}

	mustWriteWatcherFile(t, root, "updated")
	waitForRootChange(t, changes, "root-prefixed", "")
}

func mustWriteWatcherFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func waitForChange(t *testing.T, changes <-chan Change, wantHint string) {
	t.Helper()
	waitForAnyChange(t, changes, map[string]bool{wantHint: true})
}

func waitForRootChange(t *testing.T, changes <-chan Change, wantRootID, wantHint string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case change := <-changes:
			if change.RootID == wantRootID && change.RelPathHint == wantHint {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for root %q hint %q", wantRootID, wantHint)
		}
	}
}

func waitForRootChanges(t *testing.T, changes <-chan Change, want map[string]string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for len(want) > 0 {
		select {
		case change := <-changes:
			if hint, ok := want[change.RootID]; ok && change.RelPathHint == hint {
				delete(want, change.RootID)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for root changes %v", want)
		}
	}
}

func waitForAnyChange(t *testing.T, changes <-chan Change, wantHints map[string]bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case change := <-changes:
			if change.RootID == "root-1" && wantHints[change.RelPathHint] {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for any of %v", wantHints)
		}
	}
}

func assertNoWatcherChange(t *testing.T, changes <-chan Change, duration time.Duration) {
	t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case change := <-changes:
		t.Fatalf("unexpected watcher change %+v", change)
	case <-timer.C:
	}
}

func TestManagerWatchErrorTriggersFullRescanOfAllRoots(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	changes := make(chan Change, 32)
	m, err := New(func(change Change) {
		changes <- change
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()
	if err := m.AddRoot("root-a", rootA); err != nil {
		t.Fatalf("AddRoot(a): %v", err)
	}
	if err := m.AddRoot("root-b", rootB); err != nil {
		t.Fatalf("AddRoot(b): %v", err)
	}

	// Simulate an inotify failure (e.g. event queue overflow); every root
	// must receive a full-rescan change with no path hint.
	m.watcher.Errors <- errors.New("simulated overflow")

	waitForRootChanges(t, changes, map[string]string{
		"root-a": "",
		"root-b": "",
	})
}
