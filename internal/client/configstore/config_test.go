package configstore

import (
	"os"
	"path/filepath"
	"testing"

	commoncfg "syna/internal/common/config"
)

func TestSaveKeyringTightensExistingPermissions(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "config")
	keyringFile := filepath.Join(configDir, "keyring.json")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(keyringFile, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	store := New(commoncfg.ClientPaths{ConfigDir: configDir, KeyringFile: keyringFile})
	if err := store.SaveKeyring(Keyring{WorkspaceKey: "syna1-test"}); err != nil {
		t.Fatalf("SaveKeyring: %v", err)
	}
	info, err := os.Stat(keyringFile)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("keyring mode = %o want 600", got)
	}
}

func TestSaveKeyringRejectsSymlink(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "config")
	target := filepath.Join(t.TempDir(), "target.json")
	keyringFile := filepath.Join(configDir, "keyring.json")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(target): %v", err)
	}
	if err := os.Symlink(target, keyringFile); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	store := New(commoncfg.ClientPaths{ConfigDir: configDir, KeyringFile: keyringFile})
	if err := store.SaveKeyring(Keyring{WorkspaceKey: "syna1-test"}); err == nil {
		t.Fatalf("SaveKeyring should reject symlink")
	}
}
