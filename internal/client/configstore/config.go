package configstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	commoncfg "syna/internal/common/config"
)

type Config struct {
	DeviceID        string `json:"device_id"`
	DeviceName      string `json:"device_name"`
	ServerURL       string `json:"server_url,omitempty"`
	WorkspaceID     string `json:"workspace_id,omitempty"`
	DaemonAutoStart bool   `json:"daemon_auto_start"`
}

type Keyring struct {
	ServerURL    string `json:"server_url,omitempty"`
	WorkspaceID  string `json:"workspace_id,omitempty"`
	WorkspaceKey string `json:"workspace_key,omitempty"`
}

type Store struct {
	Paths commoncfg.ClientPaths
}

func New(paths commoncfg.ClientPaths) *Store {
	return &Store{Paths: paths}
}

func (s *Store) LoadConfig() (Config, error) {
	var cfg Config
	if err := loadJSONFile(s.Paths.ConfigFile, &cfg); err != nil {
		if !os.IsNotExist(err) {
			return Config{}, err
		}
		hostname, _ := os.Hostname()
		cfg = Config{
			DeviceID:        uuid.NewString(),
			DeviceName:      hostname,
			DaemonAutoStart: true,
		}
		if err := s.SaveConfig(cfg); err != nil {
			return Config{}, err
		}
	}
	if cfg.DeviceID == "" {
		cfg.DeviceID = uuid.NewString()
	}
	if cfg.DeviceName == "" {
		cfg.DeviceName, _ = os.Hostname()
	}
	return cfg, nil
}

func (s *Store) SaveConfig(cfg Config) error {
	return saveJSONFile(s.Paths.ConfigFile, cfg, 0o600)
}

func (s *Store) LoadKeyring() (Keyring, error) {
	var keyring Keyring
	if err := loadJSONFile(s.Paths.KeyringFile, &keyring); err != nil {
		if os.IsNotExist(err) {
			return Keyring{}, nil
		}
		return Keyring{}, err
	}
	return keyring, nil
}

func (s *Store) SaveKeyring(keyring Keyring) error {
	return saveJSONFile(s.Paths.KeyringFile, keyring, 0o600)
}

func (s *Store) ClearKeyring() error {
	return saveJSONFile(s.Paths.KeyringFile, Keyring{}, 0o600)
}

func loadJSONFile(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

func saveJSONFile(path string, value any, mode os.FileMode) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(append(b, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
