package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"syna/internal/common/protocol"
)

type Config struct {
	Listen            string
	DataDir           string
	PublicBaseURL     string
	SessionTTL        time.Duration
	EventRetention    time.Duration
	ZeroRefRetention  time.Duration
	LogLevel          string
	AllowHTTP         bool
	MaxEventFetchPage int
	MaxPlainChunkSize int64
	MaxEventBodyBytes int64
	MaxSnapshotBody   int64
	MaxSnapshotPlain  int64
	MaxWSClients      int
	MaxWorkspaces     int
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		Listen:            env("SYNA_LISTEN", ":8080"),
		DataDir:           env("SYNA_DATA_DIR", "/var/lib/syna"),
		PublicBaseURL:     os.Getenv("SYNA_PUBLIC_BASE_URL"),
		LogLevel:          env("SYNA_LOG_LEVEL", "info"),
		AllowHTTP:         env("SYNA_ALLOW_HTTP", "false") == "true",
		MaxEventFetchPage: 1000,
		MaxPlainChunkSize: protocol.MaxFileChunkPlainSize,
		MaxEventBodyBytes: 1 << 20,
		MaxSnapshotBody:   protocol.MaxSnapshotPlainSize,
		MaxSnapshotPlain:  protocol.MaxSnapshotPlainSize,
		MaxWSClients:      32,
		MaxWorkspaces:     0,
	}
	var err error
	if cfg.MaxWorkspaces, err = parseNonNegativeInt("SYNA_MAX_WORKSPACES", env("SYNA_MAX_WORKSPACES", "0")); err != nil {
		return Config{}, err
	}
	if cfg.SessionTTL, err = time.ParseDuration(env("SYNA_SESSION_TTL", "24h")); err != nil {
		return Config{}, fmt.Errorf("parse SYNA_SESSION_TTL: %w", err)
	}
	if cfg.EventRetention, err = time.ParseDuration(env("SYNA_EVENT_RETENTION", "24h")); err != nil {
		return Config{}, fmt.Errorf("parse SYNA_EVENT_RETENTION: %w", err)
	}
	if cfg.ZeroRefRetention, err = time.ParseDuration(env("SYNA_ZERO_REF_RETENTION", "168h")); err != nil {
		return Config{}, fmt.Errorf("parse SYNA_ZERO_REF_RETENTION: %w", err)
	}
	if cfg.ReadHeaderTimeout, err = time.ParseDuration(env("SYNA_READ_HEADER_TIMEOUT", "10s")); err != nil {
		return Config{}, fmt.Errorf("parse SYNA_READ_HEADER_TIMEOUT: %w", err)
	}
	if cfg.ReadTimeout, err = time.ParseDuration(env("SYNA_READ_TIMEOUT", "30s")); err != nil {
		return Config{}, fmt.Errorf("parse SYNA_READ_TIMEOUT: %w", err)
	}
	if cfg.WriteTimeout, err = time.ParseDuration(env("SYNA_WRITE_TIMEOUT", "30s")); err != nil {
		return Config{}, fmt.Errorf("parse SYNA_WRITE_TIMEOUT: %w", err)
	}
	if cfg.IdleTimeout, err = time.ParseDuration(env("SYNA_IDLE_TIMEOUT", "120s")); err != nil {
		return Config{}, fmt.Errorf("parse SYNA_IDLE_TIMEOUT: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func EnsureDataDirs(dataDir string) error {
	for _, dir := range []string{
		dataDir,
		filepath.Join(dataDir, "objects"),
		filepath.Join(dataDir, "tmp"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseNonNegativeInt(key, value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s must be non-negative", key)
	}
	return n, nil
}

func (cfg Config) Validate() error {
	if cfg.MaxPlainChunkSize <= 0 {
		return fmt.Errorf("MaxPlainChunkSize must be positive")
	}
	if cfg.MaxSnapshotPlain <= 0 {
		return fmt.Errorf("MaxSnapshotPlain must be positive")
	}
	if cfg.MaxWSClients <= 0 {
		return fmt.Errorf("MaxWSClients must be positive")
	}
	if cfg.MaxWorkspaces < 0 {
		return fmt.Errorf("MaxWorkspaces must be non-negative")
	}
	if cfg.PublicBaseURL == "" {
		return nil
	}
	u, err := url.Parse(cfg.PublicBaseURL)
	if err != nil {
		return fmt.Errorf("parse SYNA_PUBLIC_BASE_URL: %w", err)
	}
	if u.Scheme == "http" && !cfg.AllowHTTP {
		return fmt.Errorf("SYNA_PUBLIC_BASE_URL must use https unless SYNA_ALLOW_HTTP=true")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("SYNA_PUBLIC_BASE_URL must use http or https")
	}
	return nil
}

func (cfg Config) Warnings() []string {
	var warnings []string
	if cfg.AllowHTTP {
		warnings = append(warnings, "SYNA_ALLOW_HTTP=true enables insecure transport; use this only for local development behind loopback")
	}
	if cfg.PublicBaseURL == "" {
		warnings = append(warnings, "SYNA_PUBLIC_BASE_URL is unset; deploy behind an HTTPS reverse proxy and set the public HTTPS origin explicitly")
	} else if strings.HasPrefix(cfg.PublicBaseURL, "http://") {
		warnings = append(warnings, "SYNA_PUBLIC_BASE_URL uses http://; production clients will reject it unless SYNA_ALLOW_HTTP=true")
	}
	if strings.HasPrefix(cfg.Listen, ":") || strings.HasPrefix(cfg.Listen, "0.0.0.0:") || strings.HasPrefix(cfg.Listen, "[::]:") {
		warnings = append(warnings, "bind the backend to localhost or a private interface and expose it only through an HTTPS reverse proxy")
	}
	return warnings
}
