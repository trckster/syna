package daemon

import (
	"testing"

	"syna/internal/client/state"
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
