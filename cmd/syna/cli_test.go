package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePathArgUsesClientWorkingDirectory(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "test")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}
	t.Chdir(cwd)

	got, err := resolvePathArg("../test")
	if err != nil {
		t.Fatalf("resolvePathArg: %v", err)
	}
	want := filepath.Join(home, "test")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestResolvePathArgExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := resolvePathArg("~/docs")
	if err != nil {
		t.Fatalf("resolvePathArg: %v", err)
	}
	want := filepath.Join(home, "docs")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
