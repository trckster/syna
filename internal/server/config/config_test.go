package config

import "testing"

func TestLoadParsesMaxWorkspaces(t *testing.T) {
	clearLoadEnv(t)
	t.Setenv("SYNA_MAX_WORKSPACES", "1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxWorkspaces != 1 {
		t.Fatalf("MaxWorkspaces = %d want 1", cfg.MaxWorkspaces)
	}
}

func TestLoadDefaultsToUnlimitedWorkspaces(t *testing.T) {
	clearLoadEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxWorkspaces != 0 {
		t.Fatalf("MaxWorkspaces = %d want 0", cfg.MaxWorkspaces)
	}
}

func TestLoadRejectsNegativeMaxWorkspaces(t *testing.T) {
	clearLoadEnv(t)
	t.Setenv("SYNA_MAX_WORKSPACES", "-1")

	if _, err := Load(); err == nil {
		t.Fatalf("expected negative SYNA_MAX_WORKSPACES to be rejected")
	}
}

func clearLoadEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"SYNA_LISTEN",
		"SYNA_DATA_DIR",
		"SYNA_PUBLIC_BASE_URL",
		"SYNA_LOG_LEVEL",
		"SYNA_ALLOW_HTTP",
		"SYNA_MAX_WORKSPACES",
		"SYNA_SESSION_TTL",
		"SYNA_EVENT_RETENTION",
		"SYNA_ZERO_REF_RETENTION",
		"SYNA_READ_HEADER_TIMEOUT",
		"SYNA_READ_TIMEOUT",
		"SYNA_WRITE_TIMEOUT",
		"SYNA_IDLE_TIMEOUT",
	} {
		t.Setenv(key, "")
	}
}
