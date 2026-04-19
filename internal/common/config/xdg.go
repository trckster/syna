package config

import (
	"errors"
	"os"
	"path/filepath"
)

const (
	appName = "syna"
)

type ClientPaths struct {
	ConfigDir   string
	StateDir    string
	ConfigFile  string
	KeyringFile string
	DBFile      string
	SocketFile  string
	PIDFile     string
	SystemdDir  string
	UnitFile    string
}

func ResolveClientPaths() (ClientPaths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return ClientPaths{}, err
	}
	if home == "" {
		return ClientPaths{}, errors.New("home directory is empty")
	}

	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		stateHome = filepath.Join(home, ".local", "state")
	}

	configDir := filepath.Join(configHome, appName)
	stateDir := filepath.Join(stateHome, appName)
	systemdDir := filepath.Join(configHome, "systemd", "user")

	return ClientPaths{
		ConfigDir:   configDir,
		StateDir:    stateDir,
		ConfigFile:  filepath.Join(configDir, "config.json"),
		KeyringFile: filepath.Join(configDir, "keyring.json"),
		DBFile:      filepath.Join(stateDir, "client.db"),
		SocketFile:  filepath.Join(stateDir, "agent.sock"),
		PIDFile:     filepath.Join(stateDir, "daemon.pid"),
		SystemdDir:  systemdDir,
		UnitFile:    filepath.Join(systemdDir, "syna.service"),
	}, nil
}

func EnsureClientDirs(paths ClientPaths) error {
	for _, dir := range []string{paths.ConfigDir, paths.StateDir, paths.SystemdDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return os.Chmod(paths.StateDir, 0o700)
}
