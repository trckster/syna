package scanner

import (
	"os"
	"path/filepath"
	"testing"

	"syna/internal/common/protocol"
)

func TestScanSubtreeIncludesOnlySubtreeEntries(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a", "keep.txt"), "keep")
	mustWriteFile(t, filepath.Join(root, "b", "skip.txt"), "skip")

	result, err := ScanSubtree(root, "a")
	if err != nil {
		t.Fatalf("ScanSubtree: %v", err)
	}
	if result.RootKind != protocol.RootKindDir {
		t.Fatalf("unexpected root kind %q", result.RootKind)
	}
	var rels []string
	for _, entry := range result.Entries {
		rels = append(rels, entry.RelPath)
	}
	want := []string{"a", "a/keep.txt"}
	if len(rels) != len(want) {
		t.Fatalf("unexpected entries %v want %v", rels, want)
	}
	for i := range want {
		if rels[i] != want[i] {
			t.Fatalf("unexpected entries %v want %v", rels, want)
		}
	}
}

func TestScanSubtreeFileReturnsSingleFile(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "hello")

	result, err := ScanSubtree(root, "a.txt")
	if err != nil {
		t.Fatalf("ScanSubtree: %v", err)
	}
	if len(result.Entries) != 1 {
		t.Fatalf("unexpected entry count %d", len(result.Entries))
	}
	if got := result.Entries[0].RelPath; got != "a.txt" {
		t.Fatalf("unexpected rel path %q", got)
	}
	if result.Entries[0].Kind != protocol.RootKindFile {
		t.Fatalf("unexpected kind %q", result.Entries[0].Kind)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
