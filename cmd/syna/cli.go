package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"syna/internal/buildinfo"
	"syna/internal/client/agentrpc"
	"syna/internal/client/configstore"
	"syna/internal/client/daemon"
	commoncfg "syna/internal/common/config"
	"syna/internal/common/protocol"
)

type commandFunc func(commoncfg.ClientPaths, []string) error

var commands = map[string]commandFunc{
	"help":       helpCommand,
	"-h":         helpCommand,
	"--help":     helpCommand,
	"version":    versionCommand,
	"--version":  versionCommand,
	"-version":   versionCommand,
	"daemon":     daemonCommand,
	"connect":    connectCommand,
	"disconnect": disconnectCommand,
	"key":        keyCommand,
	"add":        addCommand,
	"rm":         removeCommand,
	"status":     statusCommand,
	"uninstall":  uninstallCommand,
}

func run(paths commoncfg.ClientPaths, args []string) error {
	if len(args) == 1 {
		usage()
		return exitCode(2)
	}
	cmd, ok := commands[args[1]]
	if !ok {
		usage()
		return exitCode(2)
	}
	return cmd(paths, args[2:])
}

func helpCommand(_ commoncfg.ClientPaths, _ []string) error {
	usage()
	return nil
}

func versionCommand(_ commoncfg.ClientPaths, _ []string) error {
	fmt.Println("syna", buildinfo.String())
	return nil
}

func daemonCommand(paths commoncfg.ClientPaths, _ []string) error {
	logger := log.New(os.Stdout, "syna ", log.LstdFlags|log.Lmsgprefix)
	d, err := daemon.New(paths, logger)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Run(context.Background())
}

func connectCommand(paths commoncfg.ClientPaths, args []string) error {
	if len(args) != 1 {
		usage()
		return exitCode(2)
	}
	socket, err := ensureSocket(paths)
	if err != nil {
		return err
	}
	req := daemon.ConnectRequest{ServerURL: args[0]}
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Recovery key (leave blank to create a new workspace): ")
	line, _ := reader.ReadString('\n')
	req.RecoveryKey = strings.TrimSpace(line)
	var resp daemon.ConnectResponse
	if err := agentrpc.Call(socket, "connect", req, &resp); err != nil {
		return err
	}
	if resp.GeneratedRecoveryKey != "" {
		fmt.Println()
		fmt.Printf("Your secret key: %s\n", resp.GeneratedRecoveryKey)
		fmt.Println()
		fmt.Println("Use it to connect other devices and don't share it with anyone!")
	} else {
		fmt.Println("Connection established!")
	}
	for _, warning := range resp.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
	}
	return nil
}

func disconnectCommand(paths commoncfg.ClientPaths, _ []string) error {
	socket, err := ensureSocket(paths)
	if err != nil {
		return err
	}
	var resp daemon.DisconnectResponse
	if err := agentrpc.Call(socket, "disconnect", nil, &resp); err != nil {
		return err
	}
	fmt.Println("Successfully disconnected this device. Local files were left untouched.")
	for _, warning := range resp.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
	}
	return nil
}

func keyCommand(paths commoncfg.ClientPaths, args []string) error {
	if len(args) != 1 || args[0] != "show" {
		usage()
		return exitCode(2)
	}
	keyring, err := configstore.New(paths).LoadKeyring()
	if err != nil {
		return err
	}
	if keyring.WorkspaceKey == "" {
		return errors.New("no recovery key is stored; connect to a workspace first")
	}
	fmt.Println(keyring.WorkspaceKey)
	return nil
}

func addCommand(paths commoncfg.ClientPaths, args []string) error {
	if len(args) != 1 {
		usage()
		return exitCode(2)
	}
	path, err := resolvePathArg(args[0])
	if err != nil {
		return err
	}
	socket, err := ensureSocket(paths)
	if err != nil {
		return err
	}
	progress := newAddProgressRenderer(os.Stderr)
	err = agentrpc.CallWithProgress(socket, "add", daemon.AddRequest{Path: path}, nil, progress.Update)
	progress.Done(err)
	return err
}

func removeCommand(paths commoncfg.ClientPaths, args []string) error {
	if len(args) != 1 {
		usage()
		return exitCode(2)
	}
	path, err := resolvePathArg(args[0])
	if err != nil {
		return err
	}
	socket, err := ensureSocket(paths)
	if err != nil {
		return err
	}
	return agentrpc.Call(socket, "rm", daemon.RemoveRequest{Path: path}, nil)
}

func resolvePathArg(input string) (string, error) {
	if input == "~" || strings.HasPrefix(input, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if input == "~" {
			input = home
		} else {
			input = filepath.Join(home, input[2:])
		}
	}
	if filepath.IsAbs(input) {
		return filepath.Clean(input), nil
	}
	return filepath.Abs(input)
}

func statusCommand(paths commoncfg.ClientPaths, _ []string) error {
	socket, err := ensureSocket(paths)
	if err != nil {
		return err
	}
	var status protocol.WorkspaceStatus
	if err := agentrpc.Call(socket, "status", nil, &status); err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(status)
}

func uninstallCommand(paths commoncfg.ClientPaths, args []string) error {
	if len(args) != 0 {
		usage()
		return exitCode(2)
	}

	var warnings []string
	if warning, err := removeUserServiceIfPresent(paths); err != nil {
		return err
	} else if warning != "" {
		warnings = append(warnings, warning)
	}
	if err := terminateRecordedDaemon(paths); err != nil {
		return err
	}
	if err := removeClientData(paths); err != nil {
		return err
	}
	removedBinary, err := removeCurrentBinary()
	if err != nil {
		return err
	}

	for _, warning := range warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
	}
	fmt.Println("Removed Syna config, keyring, local state, daemon files, and user service.")
	if removedBinary != "" {
		fmt.Printf("Removed Syna binary: %s\n", removedBinary)
	}
	fmt.Println("Synced directories were left untouched.")
	return nil
}

func usage() {
	fmt.Println(`usage:
  syna connect <server-url>  connect this device to a workspace
  syna disconnect            disconnect this device and leave local files untouched
  syna key show              print the stored workspace recovery key
  syna add <path>            add a file or directory under $HOME to sync
  syna rm <path>             stop syncing a previously added path
  syna status                print workspace, connection, warning, and root status
  syna uninstall             remove Syna config, state, daemon, service, and binary
  syna version               print the client version
  syna daemon                run the background daemon
  syna help                  show this help`)
}

func removeUserServiceIfPresent(paths commoncfg.ClientPaths) (string, error) {
	unitExists := false
	if _, err := os.Stat(paths.UnitFile); err == nil {
		unitExists = true
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("could not inspect user service: %w", err)
	}
	if !unitExists {
		return "", nil
	}

	var problems []string
	if err := runCommand(exec.Command("systemctl", "--user", "disable", "--now", "syna.service")); err != nil {
		problems = append(problems, err.Error())
	}
	if err := os.Remove(paths.UnitFile); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("remove user service %s: %w", paths.UnitFile, err)
	}
	if err := runCommand(exec.Command("systemctl", "--user", "daemon-reload")); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return fmt.Sprintf("systemd cleanup was incomplete: %s", strings.Join(problems, "; ")), nil
	}
	return "", nil
}

func terminateRecordedDaemon(paths commoncfg.ClientPaths) error {
	pidBytes, err := os.ReadFile(paths.PIDFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read daemon pid file: %w", err)
	}
	pidText := strings.TrimSpace(string(pidBytes))
	if pidText == "" {
		return nil
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return fmt.Errorf("invalid daemon pid %q", pidText)
	}
	if pid == os.Getpid() {
		return nil
	}
	if processGone(pid) {
		return nil
	}
	isDaemon, err := isSynaDaemonProcess(pid)
	if err != nil {
		return err
	}
	if !isDaemon {
		return fmt.Errorf("refusing to stop pid %d because it does not look like a Syna daemon", pid)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil && !isNoSuchProcess(err) {
		return fmt.Errorf("stop daemon pid %d: %w", pid, err)
	}
	if waitForProcessExit(pid, 3*time.Second) {
		return nil
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil && !isNoSuchProcess(err) {
		return fmt.Errorf("kill daemon pid %d: %w", pid, err)
	}
	if !waitForProcessExit(pid, 3*time.Second) {
		return fmt.Errorf("daemon pid %d did not exit", pid)
	}
	return nil
}

func isSynaDaemonProcess(pid int) (bool, error) {
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("verify daemon pid %d: %w", pid, err)
	}
	fields := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	if len(fields) < 2 {
		return false, nil
	}
	hasDaemonArg := false
	for _, field := range fields[1:] {
		if field == "daemon" {
			hasDaemonArg = true
			break
		}
	}
	if !hasDaemonArg {
		return false, nil
	}
	return filepath.Base(fields[0]) == "syna", nil
}

func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return processGone(pid)
}

func processGone(pid int) bool {
	return isNoSuchProcess(syscall.Kill(pid, 0))
}

func isNoSuchProcess(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.ESRCH)
}

func removeClientData(paths commoncfg.ClientPaths) error {
	for _, path := range []string{paths.ConfigDir, paths.StateDir} {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}

func removeCurrentBinary() (string, error) {
	execPath, err := os.Executable()
	if err != nil {
		return "", err
	}
	execPath, err = filepath.Abs(execPath)
	if err != nil {
		return "", err
	}

	candidates := []string{execPath}
	if lookedUp, err := exec.LookPath(os.Args[0]); err == nil {
		if absLookedUp, err := filepath.Abs(lookedUp); err == nil && absLookedUp != execPath {
			candidates = append(candidates, absLookedUp)
		}
	}

	var removed []string
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		if err := removeBinaryPath(candidate); err != nil {
			return "", err
		}
		removed = append(removed, candidate)
	}
	return strings.Join(removed, ", "), nil
}

func removeBinaryPath(path string) error {
	if err := os.Remove(path); err == nil || os.IsNotExist(err) {
		return nil
	} else if !os.IsPermission(err) {
		return fmt.Errorf("remove binary %s: %w", path, err)
	}

	if _, err := exec.LookPath("sudo"); err != nil {
		return fmt.Errorf("remove binary %s: permission denied; remove it with sudo", path)
	}
	if err := runCommand(exec.Command("sudo", "rm", "-f", "--", path)); err != nil {
		return fmt.Errorf("remove binary %s with sudo: %w", path, err)
	}
	return nil
}

func ensureSocket(paths commoncfg.ClientPaths) (string, error) {
	if err := commoncfg.EnsureClientDirs(paths); err != nil {
		return "", err
	}
	if err := probeSocket(paths.SocketFile); err == nil {
		return paths.SocketFile, nil
	}
	_ = os.Remove(paths.SocketFile)

	store := configstore.New(paths)
	cfg, err := store.LoadConfig()
	if err != nil {
		return "", err
	}
	if !cfg.DaemonAutoStart {
		return "", errors.New("daemon socket is absent and auto-start is disabled; run `syna daemon` manually")
	}

	if err := daemon.StartUserService(paths); err != nil {
		return "", fmt.Errorf("cannot start Syna daemon with user systemd: %w", err)
	}
	if err := waitForSocket(paths.SocketFile, 3*time.Second); err != nil {
		_ = os.Remove(paths.SocketFile)
		return "", fmt.Errorf("cannot start Syna daemon with user systemd: service started but daemon did not answer: %w", err)
	}
	return paths.SocketFile, nil
}

func waitForSocket(socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := probeSocket(socketPath); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr != nil {
		return lastErr
	}
	return errors.New("daemon socket did not appear")
}

func probeSocket(socketPath string) error {
	conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if err := json.NewEncoder(conn).Encode(agentrpc.Request{Method: "status"}); err != nil {
		return err
	}
	var resp agentrpc.Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return err
	}
	if !resp.OK {
		if resp.Error == "" {
			return errors.New("daemon status RPC failed")
		}
		return errors.New(resp.Error)
	}
	return nil
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

type exitCode int

func (e exitCode) Error() string {
	return fmt.Sprintf("exit %d", int(e))
}
