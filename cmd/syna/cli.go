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
	"strings"
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
	fmt.Print("Recovery key (leave blank to create a new workspace on a fresh server): ")
	line, _ := reader.ReadString('\n')
	req.RecoveryKey = strings.TrimSpace(line)
	var resp daemon.ConnectResponse
	if err := agentrpc.Call(socket, "connect", req, &resp); err != nil {
		return err
	}
	if resp.GeneratedRecoveryKey != "" {
		fmt.Println(resp.GeneratedRecoveryKey)
		fmt.Println("This recovery key lets other devices join the workspace.")
		fmt.Println("Anyone with it can access the encrypted workspace; store it safely.")
		fmt.Println("You can show it again on this connected device with: syna key show")
	}
	fmt.Printf("workspace: %s\n", resp.WorkspaceID)
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
	return agentrpc.Call(socket, "disconnect", nil, nil)
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
	socket, err := ensureSocket(paths)
	if err != nil {
		return err
	}
	return agentrpc.Call(socket, "add", daemon.AddRequest{Path: args[0]}, nil)
}

func removeCommand(paths commoncfg.ClientPaths, args []string) error {
	if len(args) != 1 {
		usage()
		return exitCode(2)
	}
	socket, err := ensureSocket(paths)
	if err != nil {
		return err
	}
	return agentrpc.Call(socket, "rm", daemon.RemoveRequest{Path: args[0]}, nil)
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

func usage() {
	fmt.Println(`usage:
syna connect <server-url>  connect this device to a workspace
syna disconnect            disconnect this device and leave local files untouched
syna key show              print the stored workspace recovery key
syna add <path>            add a file or directory under $HOME to sync
syna rm <path>             stop syncing a previously added path
syna status                print workspace, connection, warning, and root status
syna help                  show this help
syna -h                    show this help
syna --help                show this help
syna version               print the client version
syna --version             print the client version
syna daemon                run the background daemon`)
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
	var startupErrors []string
	if err := runCommand(exec.Command("systemctl", "--user", "start", "syna.service")); err == nil {
		if err := waitForSocket(paths.SocketFile, 3*time.Second); err == nil {
			return paths.SocketFile, nil
		} else {
			startupErrors = append(startupErrors, "systemd start succeeded but daemon did not answer: "+err.Error())
		}
	} else {
		startupErrors = append(startupErrors, err.Error())
	}

	_ = os.Remove(paths.SocketFile)
	fallback := exec.Command(os.Args[0], "daemon")
	if logFile, err := os.OpenFile(filepath.Join(paths.StateDir, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		fallback.Stdout = logFile
		fallback.Stderr = logFile
	}
	if err := fallback.Start(); err != nil {
		startupErrors = append(startupErrors, "fallback daemon launch failed: "+err.Error())
		return "", fmt.Errorf("daemon auto-start failed: %s", strings.Join(startupErrors, "; "))
	}
	if err := waitForSocket(paths.SocketFile, 5*time.Second); err != nil {
		startupErrors = append(startupErrors, fmt.Sprintf("fallback daemon pid %d did not answer: %v", fallback.Process.Pid, err))
		return "", fmt.Errorf("daemon auto-start failed: %s", strings.Join(startupErrors, "; "))
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
