package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"syna/internal/client/connector"
	"syna/internal/client/state"
	commoncrypto "syna/internal/common/crypto"
	"syna/internal/common/protocol"
)

func TestMergeRescanHints(t *testing.T) {
	cases := []struct {
		current string
		next    string
		want    string
	}{
		{current: "a/b", next: "a/c", want: "a"},
		{current: "a/b", next: "a/b/c", want: "a/b"},
		{current: "", next: "a/b", want: ""},
		{current: "a/b", next: "", want: ""},
		{current: "x", next: "y", want: ""},
	}
	for _, tc := range cases {
		if got := mergeRescanHints(tc.current, tc.next); got != tc.want {
			t.Fatalf("mergeRescanHints(%q, %q) = %q want %q", tc.current, tc.next, got, tc.want)
		}
	}
}

func TestFilterEntriesByHint(t *testing.T) {
	entries := map[string]state.Entry{
		"":           {RelPath: ""},
		"a":          {RelPath: "a"},
		"a/file":     {RelPath: "a/file"},
		"a/sub/x":    {RelPath: "a/sub/x"},
		"other/file": {RelPath: "other/file"},
	}
	filtered := filterEntriesByHint(entries, "a")
	if len(filtered) != 3 {
		t.Fatalf("unexpected filtered count %d", len(filtered))
	}
	for _, relPath := range []string{"a", "a/file", "a/sub/x"} {
		if _, ok := filtered[relPath]; !ok {
			t.Fatalf("expected %q in filtered set", relPath)
		}
	}
	if _, ok := filtered[""]; ok {
		t.Fatalf("did not expect root entry in filtered set")
	}
	if _, ok := filtered["other/file"]; ok {
		t.Fatalf("did not expect unrelated entry in filtered set")
	}
}

func TestRootForRemoveReportsUntrackedPath(t *testing.T) {
	roots := []state.Root{
		{
			RootID:        "root-test",
			Kind:          protocol.RootKindDir,
			HomeRelPath:   "test",
			TargetAbsPath: "/home/trickster/test",
			State:         protocol.RootStateActive,
		},
	}
	_, err := rootForRemove(roots, "/home/trickster/Coding/syna", "Coding/syna")
	if err == nil {
		t.Fatalf("expected untracked path error")
	}
	if strings.Contains(err.Error(), "sql: no rows") {
		t.Fatalf("leaked sql no rows error: %v", err)
	}
	if !strings.Contains(err.Error(), "not a tracked root") || !strings.Contains(err.Error(), "/home/trickster/test") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRootForRemoveReportsPathInsideTrackedRoot(t *testing.T) {
	roots := []state.Root{
		{
			RootID:        "root-test",
			Kind:          protocol.RootKindDir,
			HomeRelPath:   "test",
			TargetAbsPath: "/home/trickster/test",
			State:         protocol.RootStateActive,
		},
	}
	_, err := rootForRemove(roots, "/home/trickster/test/nested", "test/nested")
	if err == nil {
		t.Fatalf("expected inside tracked root error")
	}
	if !strings.Contains(err.Error(), "inside tracked root") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateServerURLRequiresHost(t *testing.T) {
	if err := validateServerURL("https:///missing-host"); err == nil || !strings.Contains(err.Error(), "must include a host") {
		t.Fatalf("expected missing host rejection, got %v", err)
	}
}

func TestRemoveRootRejectsWrongDirectoryWithoutChangingTrackedRoot(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	setHome(t, home)
	d, cancel := newTestDaemon(t)
	defer cancel()

	trackedDir := filepath.Join(home, "test")
	wrongDir := filepath.Join(home, "Coding", "syna")
	if err := os.MkdirAll(trackedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(tracked): %v", err)
	}
	if err := os.MkdirAll(wrongDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(wrong): %v", err)
	}
	if err := d.stateDB.UpsertRoot(state.Root{
		RootID:        "root-test",
		Kind:          protocol.RootKindDir,
		HomeRelPath:   "test",
		TargetAbsPath: trackedDir,
		State:         protocol.RootStateActive,
	}); err != nil {
		t.Fatalf("UpsertRoot: %v", err)
	}
	d.conn = connector.New("http://127.0.0.1:1")
	d.keys = &commoncrypto.DerivedKeys{}

	err := d.RemoveRoot(context.Background(), wrongDir)
	if err == nil {
		t.Fatalf("expected wrong directory to be rejected")
	}
	if !strings.Contains(err.Error(), "not a tracked root") {
		t.Fatalf("unexpected error: %v", err)
	}
	root, err := d.stateDB.RootByHomeRel("test")
	if err != nil {
		t.Fatalf("RootByHomeRel: %v", err)
	}
	if root.State != protocol.RootStateActive {
		t.Fatalf("wrong directory removed tracked root, state=%s", root.State)
	}
}
