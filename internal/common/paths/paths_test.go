package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckOverlap(t *testing.T) {
	if err := CheckOverlap("projects/wiki", []string{"projects"}); err == nil {
		t.Fatalf("expected overlap error")
	}
	if err := CheckOverlap("projects/wiki", []string{"notes"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRemoteRelPath(t *testing.T) {
	for _, tc := range []struct {
		name      string
		input     string
		allowRoot bool
		want      string
		wantErr   bool
	}{
		{name: "root entry allowed", input: "", allowRoot: true, want: ""},
		{name: "nested file", input: "docs/a.txt", want: "docs/a.txt"},
		{name: "absolute rejected", input: "/etc/passwd", wantErr: true},
		{name: "traversal rejected", input: "../etc/passwd", wantErr: true},
		{name: "non canonical rejected", input: "docs//a.txt", wantErr: true},
		{name: "empty rejected", input: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateRemoteRelPath(tc.input, tc.allowRoot)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateRemoteRelPath(%q): %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestValidateHomeRelPath(t *testing.T) {
	for _, input := range []string{"../../etc", "/root/.ssh", "notes//deep", ""} {
		if _, err := ValidateHomeRelPath(input); err == nil {
			t.Fatalf("expected error for %q", input)
		}
	}
	if got, err := ValidateHomeRelPath("notes/deep"); err != nil {
		t.Fatalf("ValidateHomeRelPath: %v", err)
	} else if got != "notes/deep" {
		t.Fatalf("got %q", got)
	}
}

func TestEnsureSafeDirRejectsSymlinkAncestor(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatalf("Mkdir(real): %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := EnsureSafeDir(root, filepath.Join(root, "link", "child"), 0o755); err == nil {
		t.Fatalf("expected symlink rejection")
	}
}
