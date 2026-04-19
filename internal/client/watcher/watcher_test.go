package watcher

import (
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
