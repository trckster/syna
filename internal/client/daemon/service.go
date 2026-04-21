package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	commoncfg "syna/internal/common/config"
)

func (d *Daemon) installUserService() error {
	return InstallUserService(d.paths)
}

func InstallUserService(paths commoncfg.ClientPaths) error {
	if err := installUserServiceUnit(paths); err != nil {
		return err
	}
	if err := runCommand(exec.Command("systemctl", "--user", "enable", "--now", "syna.service")); err != nil {
		return err
	}
	return nil
}

func StartUserService(paths commoncfg.ClientPaths) error {
	if err := installUserServiceUnit(paths); err != nil {
		return err
	}
	return runCommand(exec.Command("systemctl", "--user", "start", "syna.service"))
}

func installUserServiceUnit(paths commoncfg.ClientPaths) error {
	execPath, err := os.Executable()
	if err != nil {
		return err
	}
	unit := fmt.Sprintf(`[Unit]
Description=Syna user daemon
After=default.target

[Service]
ExecStart=%s daemon
Restart=on-failure

[Install]
WantedBy=default.target
`, execPath)
	if err := os.MkdirAll(filepath.Dir(paths.UnitFile), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(paths.UnitFile, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := runCommand(exec.Command("systemctl", "--user", "daemon-reload")); err != nil {
		return err
	}
	return nil
}

func (d *Daemon) disableUserService(ctx context.Context) error {
	return runCommand(exec.CommandContext(ctx, "systemctl", "--user", "disable", "syna.service"))
}

func runCommand(cmd *exec.Cmd) error {
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	message := strings.TrimSpace(string(out))
	if message == "" {
		return fmt.Errorf("%s: %w", strings.Join(cmd.Args, " "), err)
	}
	return fmt.Errorf("%s: %w: %s", strings.Join(cmd.Args, " "), err, message)
}
