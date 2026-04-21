package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"syna/internal/client/applier"
	"syna/internal/client/configstore"
	"syna/internal/client/connector"
	"syna/internal/client/scanner"
	"syna/internal/client/state"
	"syna/internal/client/uploader"
	"syna/internal/client/watcher"
	commoncfg "syna/internal/common/config"
	commoncrypto "syna/internal/common/crypto"
	"syna/internal/common/paths"
	"syna/internal/common/protocol"
)

type Daemon struct {
	paths    commoncfg.ClientPaths
	configs  *configstore.Store
	stateDB  *state.DB
	cfg      configstore.Config
	keyring  configstore.Keyring
	keys     *commoncrypto.DerivedKeys
	conn     *connector.Client
	logger   *log.Logger
	watcher  *watcher.Manager
	changeCh chan watcher.Change
	mu       sync.Mutex
	syncMu   sync.Mutex
	stageMu  sync.RWMutex
	shutdown context.CancelFunc
	runCtx   context.Context

	reconnectCancel       context.CancelFunc
	intentionalDisconnect bool
	reconnectLoopID       int64
	stagedRoots           map[string]struct{}
	streamingLive         bool
}

type ConnectRequest struct {
	ServerURL   string `json:"server_url"`
	RecoveryKey string `json:"recovery_key,omitempty"`
}

type ConnectResponse struct {
	WorkspaceID          string   `json:"workspace_id"`
	GeneratedRecoveryKey string   `json:"generated_recovery_key,omitempty"`
	Warnings             []string `json:"warnings,omitempty"`
}

type DisconnectResponse struct {
	Warnings []string `json:"warnings,omitempty"`
}

type AddRequest struct {
	Path string `json:"path"`
}

type RemoveRequest struct {
	Path string `json:"path"`
}

type PathConflictError struct {
	CurrentSeq int64
	PathID     string
}

func (e *PathConflictError) Error() string {
	return "path_head_mismatch"
}

type lifecycleError struct {
	kind protocol.DaemonIssueKind
	err  error
}

func (e *lifecycleError) Error() string {
	return e.err.Error()
}

func (e *lifecycleError) Unwrap() error {
	return e.err
}

func markLifecycle(kind protocol.DaemonIssueKind, err error) error {
	if err == nil {
		return nil
	}
	var existing *lifecycleError
	if errors.As(err, &existing) {
		return err
	}
	return &lifecycleError{kind: kind, err: err}
}

func lifecycleKind(err error, fallback protocol.DaemonIssueKind) protocol.DaemonIssueKind {
	var lifecycle *lifecycleError
	if errors.As(err, &lifecycle) {
		return lifecycle.kind
	}
	return fallback
}

const insecureTransportEnv = "SYNA_ALLOW_HTTP"

func New(paths commoncfg.ClientPaths, logger *log.Logger) (*Daemon, error) {
	if err := commoncfg.EnsureClientDirs(paths); err != nil {
		return nil, err
	}
	configs := configstore.New(paths)
	cfg, err := configs.LoadConfig()
	if err != nil {
		return nil, err
	}
	keyring, err := configs.LoadKeyring()
	if err != nil {
		return nil, err
	}
	stateDB, err := state.Open(paths.DBFile)
	if err != nil {
		return nil, err
	}
	if err := stateDB.Migrate(); err != nil {
		return nil, err
	}
	d := &Daemon{
		paths:       paths,
		configs:     configs,
		stateDB:     stateDB,
		cfg:         cfg,
		keyring:     keyring,
		logger:      logger,
		changeCh:    make(chan watcher.Change, 128),
		stagedRoots: make(map[string]struct{}),
	}
	d.watcher, err = watcher.New(func(change watcher.Change) {
		select {
		case d.changeCh <- change:
		default:
		}
	})
	if err != nil {
		return nil, err
	}
	if keyring.WorkspaceKey != "" {
		raw, err := commoncrypto.ParseRecoveryKey(keyring.WorkspaceKey)
		if err == nil {
			d.keys, _ = commoncrypto.Derive(raw)
		}
	}
	if cfg.ServerURL != "" {
		if err := validateServerURL(cfg.ServerURL); err != nil {
			logger.Printf("configured server URL rejected: %v", err)
		} else {
			d.conn = connector.New(cfg.ServerURL)
			if st, err := stateDB.LoadWorkspaceState(); err == nil && st.SessionToken != "" {
				d.conn = d.conn.WithToken(st.SessionToken)
			}
		}
	}
	return d, nil
}

func (d *Daemon) Close() error {
	if d.shutdown != nil {
		d.shutdown()
	}
	if d.watcher != nil {
		_ = d.watcher.Close()
	}
	return d.stateDB.Close()
}

func (d *Daemon) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	d.shutdown = cancel
	d.runCtx = ctx
	if err := os.WriteFile(d.paths.PIDFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		return err
	}
	_ = os.Remove(d.paths.SocketFile)
	ln, err := net.Listen("unix", d.paths.SocketFile)
	if err != nil {
		return err
	}
	defer func() {
		ln.Close()
		_ = os.Remove(d.paths.SocketFile)
		_ = os.Remove(d.paths.PIDFile)
	}()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	_ = os.Chmod(d.paths.SocketFile, 0o600)

	d.mu.Lock()
	if d.cfg.ServerURL != "" && d.keyring.WorkspaceKey != "" {
		d.startReconnectLoopLocked()
	}
	d.mu.Unlock()
	go d.watchLoop(ctx)
	go d.blockedRetryLoop(ctx)

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		go d.handleConn(conn)
	}
}

func (d *Daemon) Connect(ctx context.Context, req ConnectRequest) (*ConnectResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.cfg.ServerURL != "" && d.cfg.ServerURL != req.ServerURL {
		return nil, fmt.Errorf("already connected to %s; run `syna disconnect` first", d.cfg.ServerURL)
	}
	serverURL := strings.TrimRight(req.ServerURL, "/")
	if err := validateServerURL(serverURL); err != nil {
		return nil, err
	}

	var (
		displayKey string
		rawKey     []byte
		err        error
	)
	createIfMissing := false
	if req.RecoveryKey != "" {
		displayKey = req.RecoveryKey
		rawKey, err = commoncrypto.ParseRecoveryKey(req.RecoveryKey)
		if err != nil {
			return nil, err
		}
	} else {
		displayKey, rawKey, err = commoncrypto.GenerateRecoveryKey()
		if err != nil {
			return nil, err
		}
		createIfMissing = true
	}
	keys, err := commoncrypto.Derive(rawKey)
	if err != nil {
		return nil, err
	}
	workspaceID := commoncrypto.WorkspaceID(keys)

	session, err := authenticateSession(ctx, sessionHandshake{
		ServerURL:       serverURL,
		WorkspaceID:     workspaceID,
		DeviceID:        d.cfg.DeviceID,
		DeviceName:      d.cfg.DeviceName,
		Keys:            keys,
		CreateIfMissing: createIfMissing,
	})
	if err != nil {
		return nil, err
	}

	d.cfg.ServerURL = serverURL
	d.cfg.WorkspaceID = workspaceID
	d.keyring = configstore.Keyring{
		ServerURL:    serverURL,
		WorkspaceID:  workspaceID,
		WorkspaceKey: displayKey,
	}
	d.keys = keys
	d.conn = session.Client

	if err := d.configs.SaveConfig(d.cfg); err != nil {
		return nil, err
	}
	if err := d.configs.SaveKeyring(d.keyring); err != nil {
		return nil, err
	}
	initialState := protocol.ConnectionLive
	if session.CurrentSeq > 0 {
		initialState = protocol.ConnectionNeedsBootstrap
	}
	if err := d.stateDB.SaveWorkspaceState(state.WorkspaceState{
		ServerURL:        serverURL,
		WorkspaceID:      workspaceID,
		SessionToken:     session.Token,
		SessionExpiresAt: session.ExpiresAt,
		LastServerSeq:    0,
		ConnectionState:  initialState,
	}); err != nil {
		return nil, err
	}
	var warnings []string
	if d.cfg.DaemonAutoStart {
		if err := d.installUserService(); err != nil {
			warning := "background service could not be enabled: " + err.Error()
			warnings = append(warnings, warning)
			_ = d.stateDB.UpsertWarning("service:install", warning, time.Now().UTC())
		} else {
			_ = d.stateDB.ClearWarningsWithPrefix("service:install")
		}
	}
	if session.CurrentSeq > 0 {
		if err := d.bootstrap(ctx); err != nil {
			_ = d.stateDB.SetConnectionStateWithKind(protocol.ConnectionDegraded, protocol.IssueBootstrap, err.Error())
			return nil, err
		}
		_ = d.stateDB.SetConnectionState(protocol.ConnectionLive, "")
	}
	d.intentionalDisconnect = false
	d.startReconnectLoopLocked()
	return &ConnectResponse{
		WorkspaceID:          workspaceID,
		GeneratedRecoveryKey: map[bool]string{true: displayKey, false: ""}[createIfMissing],
		Warnings:             warnings,
	}, nil
}

func (d *Daemon) Disconnect(ctx context.Context) error {
	_, err := d.DisconnectWithResponse(ctx)
	return err
}

func (d *Daemon) DisconnectWithResponse(ctx context.Context) (*DisconnectResponse, error) {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	roots, _ := d.stateDB.ListRoots()
	for _, root := range roots {
		d.watcher.RemoveRoot(root.RootID)
	}
	d.intentionalDisconnect = true
	if d.reconnectCancel != nil {
		d.reconnectCancel()
		d.reconnectCancel = nil
	}
	d.cfg.ServerURL = ""
	d.cfg.WorkspaceID = ""
	d.keyring = configstore.Keyring{}
	d.keys = nil
	d.conn = nil
	if err := d.configs.SaveConfig(d.cfg); err != nil {
		return nil, err
	}
	if err := d.configs.ClearKeyring(); err != nil {
		return nil, err
	}
	if err := d.stateDB.ClearWorkspace(); err != nil {
		return nil, err
	}
	var warnings []string
	if d.cfg.DaemonAutoStart {
		if err := d.disableUserService(ctx); err != nil {
			warning := "background service could not be disabled: " + err.Error()
			warnings = append(warnings, warning)
			_ = d.stateDB.UpsertWarning("service:disable", warning, time.Now().UTC())
		} else {
			_ = d.stateDB.ClearWarningsWithPrefix("service:disable")
		}
	}
	return &DisconnectResponse{Warnings: warnings}, nil
}

func (d *Daemon) stopAfterDisconnect() {
	if d.shutdown != nil {
		d.shutdown()
	}
}

func (d *Daemon) AddRoot(ctx context.Context, input string) error {
	return d.AddRootWithProgress(ctx, input, nil)
}

func (d *Daemon) AddRootWithProgress(ctx context.Context, input string, progress AddProgressFunc) error {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil || d.keys == nil || d.cfg.WorkspaceID == "" {
		return errors.New("not connected")
	}
	roots, err := d.stateDB.ListRoots()
	if err != nil {
		return err
	}
	var existing []string
	for _, root := range roots {
		if root.State != protocol.RootStateRemoved {
			existing = append(existing, root.HomeRelPath)
		}
	}
	absPath, homeRelPath, err := paths.CanonicalizeRootPath(input, existing)
	if err != nil {
		return err
	}
	info, err := os.Lstat(absPath)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("symlink roots are not supported")
	}
	rootID := commoncrypto.RootID(d.keys, homeRelPath)
	reportAddProgress(progress, AddProgress{
		Stage:   "scanning",
		Message: "counting files",
		Path:    homeRelPath,
	})
	scan, err := scanner.ScanRoot(absPath)
	if err != nil {
		return err
	}
	totals := addProgressTotals(scan)
	reportAddProgress(progress, AddProgress{
		Stage:        "syncing",
		Message:      "starting upload",
		Path:         homeRelPath,
		TotalBytes:   totals.TotalBytes,
		TotalFiles:   totals.TotalFiles,
		TotalEntries: totals.TotalEntries,
	})

	root := state.Root{
		RootID:        rootID,
		Kind:          scan.RootKind,
		HomeRelPath:   homeRelPath,
		TargetAbsPath: absPath,
		State:         protocol.RootStateActive,
	}
	if err := d.stateDB.UpsertRoot(root); err != nil {
		return err
	}
	if err := d.recordScanWarnings(root, scan.Warnings); err != nil {
		return err
	}

	rootAddPayload := protocol.RootAddPayload{
		RootID:      rootID,
		Kind:        scan.RootKind,
		HomeRelPath: homeRelPath,
	}
	if _, err := d.submitEvent(ctx, rootID, "", scan.RootKind, protocol.EventRootAdd, nil, rootAddPayload, nil); err != nil {
		return err
	}

	initialSync, err := d.submitInitialRootEntries(ctx, rootID, homeRelPath, scan, progress, totals)
	if err != nil {
		return err
	}
	if err := d.stateDB.ReplaceEntries(rootID, initialSync.Entries); err != nil {
		return err
	}
	reportAddProgress(progress, AddProgress{
		Stage:        "finalizing",
		Message:      "publishing snapshot",
		Path:         homeRelPath,
		DoneBytes:    totals.DoneBytes,
		TotalBytes:   totals.TotalBytes,
		DoneFiles:    totals.DoneFiles,
		TotalFiles:   totals.TotalFiles,
		DoneEntries:  totals.DoneEntries,
		TotalEntries: totals.TotalEntries,
	})
	d.publishInitialSnapshot(ctx, rootID, initialSync)
	d.addWatcherRoot(rootID, absPath)
	reportAddProgress(progress, AddProgress{
		Stage:        "done",
		Message:      "sync complete",
		Path:         homeRelPath,
		DoneBytes:    totals.TotalBytes,
		TotalBytes:   totals.TotalBytes,
		DoneFiles:    totals.TotalFiles,
		TotalFiles:   totals.TotalFiles,
		DoneEntries:  totals.TotalEntries,
		TotalEntries: totals.TotalEntries,
	})
	return nil
}

func (d *Daemon) RemoveRoot(ctx context.Context, input string) error {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil || d.keys == nil {
		return errors.New("not connected")
	}
	absPath, homeRelPath, err := paths.CanonicalizeRootPath(input, nil)
	if err != nil {
		return err
	}
	roots, err := d.stateDB.ListRoots()
	if err != nil {
		return err
	}
	root, err := rootForRemove(roots, absPath, homeRelPath)
	if err != nil {
		return err
	}
	if _, err := d.submitEvent(ctx, root.RootID, "", "", protocol.EventRootRemove, nil, protocol.RootRemovePayload{RootID: root.RootID}, nil); err != nil {
		return err
	}
	return d.markRootRemoved(*root)
}

func rootForRemove(roots []state.Root, absPath, homeRelPath string) (*state.Root, error) {
	cleanAbs := filepath.Clean(absPath)
	for i := range roots {
		root := &roots[i]
		if cleanStoredRootPath(root.HomeRelPath) != cleanStoredRootPath(homeRelPath) && filepath.Clean(root.TargetAbsPath) != cleanAbs {
			continue
		}
		if root.State == protocol.RootStateRemoved {
			return nil, fmt.Errorf("path %q is already removed from sync", cleanAbs)
		}
		return root, nil
	}
	for i := range roots {
		root := roots[i]
		if root.State == protocol.RootStateRemoved || root.Kind != protocol.RootKindDir {
			continue
		}
		if pathWithinRoot(root.TargetAbsPath, cleanAbs) {
			return nil, fmt.Errorf("path %q is inside tracked root %q; syna rm only removes tracked roots", cleanAbs, filepath.Clean(root.TargetAbsPath))
		}
	}
	active := activeRootTargets(roots)
	if len(active) == 0 {
		return nil, fmt.Errorf("path %q is not a tracked root", cleanAbs)
	}
	return nil, fmt.Errorf("path %q is not a tracked root; tracked roots: %s", cleanAbs, strings.Join(active, ", "))
}

func cleanStoredRootPath(path string) string {
	return strings.Trim(filepath.ToSlash(filepath.Clean(path)), "/")
}

func pathWithinRoot(rootAbs, absPath string) bool {
	root := filepath.Clean(rootAbs)
	path := filepath.Clean(absPath)
	return path == root || strings.HasPrefix(path, root+string(os.PathSeparator))
}

func activeRootTargets(roots []state.Root) []string {
	var targets []string
	for _, root := range roots {
		if root.State == protocol.RootStateRemoved {
			continue
		}
		targets = append(targets, filepath.Clean(root.TargetAbsPath))
	}
	sort.Strings(targets)
	const maxTargets = 5
	if len(targets) > maxTargets {
		remaining := len(targets) - maxTargets
		targets = append(targets[:maxTargets], fmt.Sprintf("and %d more", remaining))
	}
	return targets
}

func (d *Daemon) Status() (*protocol.WorkspaceStatus, error) {
	st, err := d.stateDB.LoadWorkspaceState()
	if err != nil {
		return nil, err
	}
	roots, err := d.stateDB.ListRoots()
	if err != nil {
		return nil, err
	}
	pending, _ := d.stateDB.CountPendingOps()
	warnings, _ := d.stateDB.ListWarnings()
	status := &protocol.WorkspaceStatus{
		ServerURL:     st.ServerURL,
		WorkspaceID:   st.WorkspaceID,
		Connection:    st.ConnectionState,
		LastServerSeq: st.LastServerSeq,
		PendingOps:    pending,
		LastErrorKind: st.LastErrorKind,
		LastError:     st.LastError,
	}
	if st.LastError != "" {
		kind := st.LastErrorKind
		if kind == "" {
			kind = protocol.IssueTransport
		}
		status.Issues = append(status.Issues, protocol.DaemonIssue{Kind: kind, Message: st.LastError})
	}
	for _, warning := range warnings {
		status.Warnings = append(status.Warnings, warning.Message)
		status.Issues = append(status.Issues, protocol.DaemonIssue{
			Kind:    warningIssueKind(warning.Key),
			Message: warning.Message,
		})
	}
	hasBlockedRoot := false
	for _, root := range roots {
		if root.State == protocol.RootStateBlockedNonEmpty {
			hasBlockedRoot = true
		}
		status.TrackedRoots = append(status.TrackedRoots, protocol.RootStatus{
			RootID:        root.RootID,
			Kind:          root.Kind,
			HomeRelPath:   root.HomeRelPath,
			TargetAbsPath: root.TargetAbsPath,
			State:         root.State,
		})
	}
	if hasBlockedRoot && status.Connection != protocol.ConnectionDisconnected && status.Connection != protocol.ConnectionDegraded {
		status.Connection = protocol.ConnectionBlockedNonEmpty
	}
	return status, nil
}

func (d *Daemon) submitEvent(ctx context.Context, rootID, pathID string, rootKind protocol.RootKind, eventType protocol.EventType, baseSeq *int64, payload any, objectRefs []string) (*protocol.EventSubmitResponse, error) {
	plain, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	blob, err := commoncrypto.Encrypt(d.keys.EventKey, plain, commoncrypto.EventAAD(d.cfg.WorkspaceID, rootID, pathID, string(eventType)))
	if err != nil {
		return nil, err
	}
	req := protocol.EventSubmitRequest{
		RootID:      rootID,
		PathID:      pathID,
		RootKind:    rootKind,
		EventType:   eventType,
		BaseSeq:     baseSeq,
		PayloadBlob: commoncrypto.Base64Raw(blob),
		ObjectRefs:  objectRefs,
	}
	resp, apiErr, err := d.conn.SubmitEvent(ctx, req)
	if err != nil {
		if apiErr != nil && apiErr.Code == "path_head_mismatch" {
			return nil, &PathConflictError{CurrentSeq: apiErr.CurrentSeq, PathID: pathID}
		}
		if err.Error() == "path_head_mismatch" {
			return nil, &PathConflictError{PathID: pathID}
		}
		if apiErr != nil && apiErr.Code != "" {
			return nil, fmt.Errorf("%s", apiErr.Code)
		}
		return nil, err
	}
	_ = d.stateDB.AdvanceLastSeq(resp.AcceptedSeq)
	return resp, nil
}

func (d *Daemon) reconnectLoop(ctx context.Context) {
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := d.syncAndStream(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			_ = d.stateDB.SetConnectionStateWithKind(protocol.ConnectionDegraded, lifecycleKind(err, protocol.IssueTransport), err.Error())
			sleep := jitter(backoff)
			timer := time.NewTimer(sleep)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if backoff < 60*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (d *Daemon) syncAndStream(ctx context.Context) error {
	d.syncMu.Lock()
	syncLocked := true
	defer func() {
		if syncLocked {
			d.syncMu.Unlock()
		}
	}()

	d.mu.Lock()
	conn := d.conn
	keys := d.keys
	workspaceID := d.cfg.WorkspaceID
	intentionalDisconnect := d.intentionalDisconnect
	d.mu.Unlock()
	if intentionalDisconnect || conn == nil || keys == nil || workspaceID == "" {
		return nil
	}
	if err := d.ensureSession(ctx); err != nil {
		return markLifecycle(protocol.IssueAuth, err)
	}
	if err := d.stateDB.SetConnectionState(protocol.ConnectionCatchingUp, ""); err != nil {
		return err
	}
	if err := d.flushPendingOps(ctx); err != nil {
		return markLifecycle(protocol.IssueTransport, err)
	}
	if err := d.bootstrapOrCatchUp(ctx); err != nil {
		return markLifecycle(protocol.IssueBootstrap, err)
	}
	if err := d.retryBlockedRoots(ctx); err != nil {
		return markLifecycle(protocol.IssueBootstrap, err)
	}
	if err := d.reconcileActiveRoots(ctx); err != nil {
		return markLifecycle(protocol.IssueTransport, err)
	}
	if err := d.stateDB.SetConnectionState(protocol.ConnectionLive, ""); err != nil {
		return err
	}
	d.syncMu.Unlock()
	syncLocked = false

	d.setStreamingLive(true)
	defer d.setStreamingLive(false)
	ws, err := conn.DialWS(ctx)
	if err != nil {
		return markLifecycle(protocol.IssueTransport, err)
	}
	defer ws.Close()
	for {
		var msg protocol.WSMessage
		if err := ws.ReadJSON(&msg); err != nil {
			return err
		}
		if msg.Type == "event" && msg.Event != nil {
			if err := d.applyRemoteEvent(ctx, *msg.Event); err != nil {
				d.logger.Printf("apply remote event: %v", err)
			}
		}
	}
}

func (d *Daemon) bootstrapOrCatchUp(ctx context.Context) error {
	st, err := d.stateDB.LoadWorkspaceState()
	if err != nil {
		return err
	}
	resp, apiErr, err := d.conn.FetchEvents(ctx, st.LastServerSeq, 1000)
	if err != nil && apiErr != nil && apiErr.Code == "resync_required" {
		return d.bootstrap(ctx)
	}
	if err != nil {
		return err
	}
	for _, event := range resp.Events {
		if err := d.applyRemoteEvent(ctx, event); err != nil {
			return err
		}
	}
	return d.stateDB.AdvanceLastSeq(resp.CurrentSeq)
}

func (d *Daemon) bootstrap(ctx context.Context) error {
	resp, err := d.conn.Bootstrap(ctx)
	if err != nil {
		return err
	}
	for _, root := range resp.Roots {
		if _, err := d.ensureRootFromDescriptor(root); err != nil {
			var integrityErr *applier.IntegrityError
			if errors.As(err, &integrityErr) {
				d.logger.Printf("rejected bootstrap root %s: %v", root.RootID, integrityErr)
				continue
			}
			return err
		}
		if root.LatestSnapshotObjectID != "" && root.LatestSnapshotSeq > 0 {
			localRoot, err := d.stateDB.RootByID(root.RootID)
			if err != nil {
				return err
			}
			if localRoot.State == protocol.RootStateActive {
				if err := d.applySnapshot(ctx, *localRoot, root.LatestSnapshotObjectID, root.LatestSnapshotSeq, d.isRootStaged(root.RootID)); err != nil {
					var integrityErr *applier.IntegrityError
					if errors.As(err, &integrityErr) {
						d.logger.Printf("rejected bootstrap snapshot for root %s: %v", root.RootID, integrityErr)
						continue
					}
					return err
				}
			}
		}
	}
	events, _, err := d.conn.FetchEvents(ctx, resp.BootstrapAfterSeq, 1000)
	if err != nil {
		return err
	}
	for _, event := range events.Events {
		if err := d.applyRemoteEvent(ctx, event); err != nil {
			return err
		}
	}
	roots, _ := d.stateDB.ListRoots()
	for _, root := range roots {
		if root.State == protocol.RootStateActive && !d.isRootStaged(root.RootID) {
			d.addWatcherRoot(root.RootID, root.TargetAbsPath)
		}
	}
	return d.stateDB.AdvanceLastSeq(resp.CurrentSeq)
}

func (d *Daemon) ensureRootFromDescriptor(root protocol.BootstrapRoot) (bool, error) {
	blob, err := commoncrypto.ParseBase64Raw(root.DescriptorBlob)
	if err != nil {
		return false, err
	}
	plain, err := commoncrypto.Decrypt(d.keys.EventKey, blob, commoncrypto.EventAAD(d.cfg.WorkspaceID, root.RootID, "", string(protocol.EventRootAdd)))
	if err != nil {
		return false, err
	}
	var payload protocol.RootAddPayload
	if err := json.Unmarshal(plain, &payload); err != nil {
		return false, err
	}
	homeRel, target, err := paths.ResolveHomeRelTarget(payload.HomeRelPath)
	if err != nil {
		return false, &applier.IntegrityError{Message: "rejected remote root_add with invalid home_rel_path"}
	}
	if payload.RootID != root.RootID {
		return false, &applier.IntegrityError{Message: "rejected remote root_add with mismatched root_id"}
	}
	if expected := commoncrypto.RootID(d.keys, homeRel); expected != root.RootID {
		return false, &applier.IntegrityError{Message: "rejected remote root_add with invalid root_id binding"}
	}
	if payload.Kind != root.Kind {
		return false, &applier.IntegrityError{Message: "rejected remote root_add with mismatched root kind"}
	}
	existing, err := d.stateDB.RootByID(payload.RootID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if existing != nil &&
		existing.State == protocol.RootStateActive &&
		existing.Kind == payload.Kind &&
		existing.HomeRelPath == homeRel &&
		filepath.Clean(existing.TargetAbsPath) == filepath.Clean(target) {
		if err := d.stateDB.UpsertRoot(state.Root{
			RootID:            payload.RootID,
			Kind:              payload.Kind,
			HomeRelPath:       homeRel,
			TargetAbsPath:     target,
			State:             protocol.RootStateActive,
			LatestSnapshotSeq: root.LatestSnapshotSeq,
		}); err != nil {
			return false, err
		}
		return false, nil
	}
	reactivation := existing != nil && existing.State == protocol.RootStateRemoved && existing.TargetAbsPath == target
	if payload.Kind == protocol.RootKindDir {
		if info, err := os.Lstat(target); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				d.clearStagedRoot(payload.RootID)
				return false, d.stateDB.UpsertRoot(state.Root{
					RootID:            payload.RootID,
					Kind:              payload.Kind,
					HomeRelPath:       homeRel,
					TargetAbsPath:     target,
					State:             protocol.RootStateBlockedNonEmpty,
					LatestSnapshotSeq: root.LatestSnapshotSeq,
				})
			}
			if !reactivation {
				entries, _ := os.ReadDir(target)
				if len(entries) > 0 {
					d.clearStagedRoot(payload.RootID)
					return false, d.stateDB.UpsertRoot(state.Root{
						RootID:            payload.RootID,
						Kind:              payload.Kind,
						HomeRelPath:       homeRel,
						TargetAbsPath:     target,
						State:             protocol.RootStateBlockedNonEmpty,
						LatestSnapshotSeq: root.LatestSnapshotSeq,
					})
				}
			}
		}
		home, err := paths.HomeDir()
		if err != nil {
			return false, err
		}
		if err := paths.EnsureSafeDir(home, target, 0o755); err != nil {
			return false, err
		}
	} else {
		if info, err := os.Lstat(target); err == nil {
			if !reactivation {
				d.clearStagedRoot(payload.RootID)
				return false, d.stateDB.UpsertRoot(state.Root{
					RootID:            payload.RootID,
					Kind:              payload.Kind,
					HomeRelPath:       homeRel,
					TargetAbsPath:     target,
					State:             protocol.RootStateBlockedNonEmpty,
					LatestSnapshotSeq: root.LatestSnapshotSeq,
				})
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return false, &applier.IntegrityError{Message: "rejected remote root_add targeting a symlink"}
			}
		}
		home, err := paths.HomeDir()
		if err != nil {
			return false, err
		}
		if err := paths.EnsureSafeDir(home, filepath.Dir(target), 0o755); err != nil {
			return false, err
		}
	}
	if err := d.stateDB.UpsertRoot(state.Root{
		RootID:            payload.RootID,
		Kind:              payload.Kind,
		HomeRelPath:       homeRel,
		TargetAbsPath:     target,
		State:             protocol.RootStateActive,
		LatestSnapshotSeq: root.LatestSnapshotSeq,
	}); err != nil {
		return false, err
	}
	if reactivation {
		d.markStagedRoot(payload.RootID)
	} else {
		d.clearStagedRoot(payload.RootID)
	}
	return reactivation, nil
}

func (d *Daemon) applyRemoteEvent(ctx context.Context, event protocol.EventRecord) error {
	root, err := d.stateDB.RootByID(event.RootID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	switch event.EventType {
	case protocol.EventRootAdd:
		var payload protocol.RootAddPayload
		blob, err := commoncrypto.ParseBase64Raw(event.PayloadBlob)
		if err != nil {
			return err
		}
		plain, err := commoncrypto.Decrypt(d.keys.EventKey, blob, commoncrypto.EventAAD(d.cfg.WorkspaceID, event.RootID, "", string(protocol.EventRootAdd)))
		if err != nil {
			return err
		}
		if err := json.Unmarshal(plain, &payload); err != nil {
			return err
		}
		staged, err := d.ensureRootFromDescriptor(protocol.BootstrapRoot{
			RootID:         payload.RootID,
			Kind:           payload.Kind,
			DescriptorBlob: event.PayloadBlob,
		})
		if err != nil {
			var integrityErr *applier.IntegrityError
			if errors.As(err, &integrityErr) {
				d.logRemoteIntegrityError(event, integrityErr)
				return d.stateDB.AdvanceLastSeq(event.Seq)
			}
			return err
		}
		root, err = d.stateDB.RootByID(event.RootID)
		if err != nil {
			return err
		}
		if root.State == protocol.RootStateActive && !staged {
			d.addWatcherRoot(payload.RootID, root.TargetAbsPath)
		}
		if staged && d.isStreamingLive() {
			d.enqueueRootRescan(event.RootID)
		}
		return nil
	case protocol.EventRootRemove:
		if root == nil {
			return nil
		}
		if err := d.markRootRemoved(*root); err != nil {
			return err
		}
		return d.stateDB.AdvanceLastSeq(event.Seq)
	default:
		if root == nil || root.State != protocol.RootStateActive {
			return nil
		}
		staged := d.isRootStaged(root.RootID)
		if err := applier.ApplyEvent(ctx, d.conn, d.keys, d.cfg.WorkspaceID, *root, event, d.stateDB, applier.ApplyOptions{StageOnly: staged}); err != nil {
			var integrityErr *applier.IntegrityError
			if errors.As(err, &integrityErr) {
				d.logRemoteIntegrityError(event, integrityErr)
				return d.stateDB.AdvanceLastSeq(event.Seq)
			}
			return err
		}
		if staged && d.isStreamingLive() {
			d.enqueueRootRescan(root.RootID)
		}
		return d.stateDB.AdvanceLastSeq(event.Seq)
	}
}

func (d *Daemon) watchLoop(ctx context.Context) {
	var debounceMu sync.Mutex
	debouncedHints := map[string]string{}
	debounced := map[string]*time.Timer{}
	for {
		select {
		case <-ctx.Done():
			return
		case change := <-d.changeCh:
			rootID := change.RootID
			debounceMu.Lock()
			if currentHint, ok := debouncedHints[rootID]; ok {
				debouncedHints[rootID] = mergeRescanHints(currentHint, change.RelPathHint)
			} else {
				debouncedHints[rootID] = normalizeRescanHint(change.RelPathHint)
			}
			if timer := debounced[rootID]; timer != nil {
				timer.Stop()
			}
			debounced[rootID] = time.AfterFunc(500*time.Millisecond, func() {
				debounceMu.Lock()
				hint := debouncedHints[rootID]
				delete(debouncedHints, rootID)
				debounceMu.Unlock()
				d.syncMu.Lock()
				defer d.syncMu.Unlock()
				d.mu.Lock()
				defer d.mu.Unlock()
				if err := d.rescanRootHint(ctx, rootID, hint); err != nil {
					d.logger.Printf("rescan %s (%s): %v", rootID, hint, err)
				}
			})
			debounceMu.Unlock()
		}
	}
}

func (d *Daemon) rescanRoot(ctx context.Context, rootID string) error {
	return d.rescanRootHint(ctx, rootID, "")
}

func (d *Daemon) rescanRootHint(ctx context.Context, rootID, relPathHint string) error {
	return d.rescanRootHintWithRetry(ctx, rootID, relPathHint, true, true)
}

func (d *Daemon) rescanRootWithRetry(ctx context.Context, rootID string, allowRetry bool, queueRetryable bool) error {
	return d.rescanRootHintWithRetry(ctx, rootID, "", allowRetry, queueRetryable)
}

func (d *Daemon) rescanRootHintWithRetry(ctx context.Context, rootID, relPathHint string, allowRetry bool, queueRetryable bool) error {
	root, err := d.stateDB.RootByID(rootID)
	if err != nil {
		return err
	}
	if root.State != protocol.RootStateActive {
		return nil
	}
	subtreeHint := normalizeRescanHint(relPathHint)
	scan, err := scanner.ScanSubtree(root.TargetAbsPath, subtreeHint)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if errors.Is(err, os.ErrNotExist) {
		if root.Kind == protocol.RootKindDir && rootPathMissing(root.TargetAbsPath) {
			return d.handleDeletedRoot(ctx, *root, queueRetryable)
		}
		scan = &scanner.Result{RootKind: root.Kind}
	}
	if scan != nil {
		if err := d.recordScanWarnings(*root, scan.Warnings); err != nil {
			return err
		}
	}
	current, err := d.stateDB.EntriesForRoot(rootID)
	if err != nil {
		return err
	}
	current = filterEntriesByHint(current, subtreeHint)
	next := make(map[string]scanner.Entry)
	for _, item := range scan.Entries {
		next[item.RelPath] = item
	}
	for relPath, old := range current {
		if _, ok := next[relPath]; ok {
			continue
		}
		if old.Deleted {
			continue
		}
		suppressDelete, err := d.shouldSuppressIgnoredDelete(rootID, relPath, time.Now().UTC())
		if err != nil {
			return err
		}
		if suppressDelete {
			continue
		}
		payload := protocol.DeletePayload{Path: relPath}
		resp, err := d.submitEvent(ctx, rootID, old.PathID, "", protocol.EventDelete, &old.CurrentSeq, payload, nil)
		if err != nil {
			if allowRetry {
				var conflict *PathConflictError
				if errors.As(err, &conflict) {
					if err := d.applyCurrentRemoteHead(ctx, conflict.CurrentSeq); err != nil {
						return err
					}
					return d.rescanRootHintWithRetry(ctx, rootID, subtreeHint, false, queueRetryable)
				}
			}
			if queueRetryable && isRetryableSyncError(err) {
				return d.queuePendingRootRescan(rootID, err)
			}
			return err
		}
		if err := d.stateDB.MarkEntryDeleted(state.Entry{
			RootID:     rootID,
			RelPath:    relPath,
			PathID:     old.PathID,
			Kind:       old.Kind,
			CurrentSeq: resp.AcceptedSeq,
		}); err != nil {
			return err
		}
	}
	for _, item := range scan.Entries {
		pathID := commoncrypto.PathID(d.keys, rootID, item.RelPath)
		old, ok := current[item.RelPath]
		baseSeq := int64(0)
		if ok {
			baseSeq = old.CurrentSeq
		}
		if item.Kind == protocol.RootKindDir {
			if ok && !old.Deleted && old.Kind == protocol.RootKindDir && old.Mode == item.Mode && old.MTimeNS == item.MTimeNS {
				continue
			}
			resp, err := d.submitEvent(ctx, rootID, pathID, "", protocol.EventDirPut, &baseSeq, protocol.DirPutPayload{
				Path:    item.RelPath,
				Mode:    item.Mode,
				MTimeNS: item.MTimeNS,
			}, nil)
			if err != nil {
				var conflict *PathConflictError
				if errors.As(err, &conflict) {
					if err := d.applyCurrentRemoteHead(ctx, conflict.CurrentSeq); err != nil {
						return err
					}
					if allowRetry {
						return d.rescanRootHintWithRetry(ctx, rootID, subtreeHint, false, queueRetryable)
					}
					continue
				}
				if queueRetryable && isRetryableSyncError(err) {
					return d.queuePendingRootRescan(rootID, err)
				}
				return err
			}
			if err := d.stateDB.UpsertEntry(state.Entry{
				RootID:     rootID,
				RelPath:    item.RelPath,
				PathID:     pathID,
				Kind:       protocol.RootKindDir,
				CurrentSeq: resp.AcceptedSeq,
				Mode:       item.Mode,
				MTimeNS:    item.MTimeNS,
			}); err != nil {
				return err
			}
			continue
		}
		if ok && !old.Deleted && old.Kind == protocol.RootKindFile && old.ContentSHA256 == item.ContentSHA256 && old.Mode == item.Mode && old.MTimeNS == item.MTimeNS {
			continue
		}
		suppressEntry, err := d.shouldSuppressIgnoredEntry(rootID, item, time.Now().UTC())
		if err != nil {
			return err
		}
		if suppressEntry {
			continue
		}
		up, err := uploader.UploadFile(ctx, d.conn, d.keys.BlobKey, d.cfg.WorkspaceID, rootID, pathID, item.RelPath, item.AbsPath, item.Mode, item.MTimeNS)
		if err != nil {
			if queueRetryable && isRetryableSyncError(err) {
				return d.queuePendingRootRescan(rootID, err)
			}
			return err
		}
		resp, err := d.submitEvent(ctx, rootID, pathID, "", protocol.EventFilePut, &baseSeq, up.Payload, up.Refs)
		if err != nil {
			if allowRetry {
				var conflict *PathConflictError
				if errors.As(err, &conflict) {
					if err := d.resolveFileConflict(ctx, *root, item, conflict); err != nil {
						return err
					}
					return d.rescanRootHintWithRetry(ctx, rootID, subtreeHint, false, queueRetryable)
				}
			}
			if queueRetryable && isRetryableSyncError(err) {
				return d.queuePendingRootRescan(rootID, err)
			}
			return err
		}
		if err := d.stateDB.UpsertEntry(state.Entry{
			RootID:        rootID,
			RelPath:       item.RelPath,
			PathID:        pathID,
			Kind:          protocol.RootKindFile,
			CurrentSeq:    resp.AcceptedSeq,
			ContentSHA256: up.Payload.ContentSHA256,
			SizeBytes:     up.Payload.SizeBytes,
			Mode:          up.Payload.Mode,
			MTimeNS:       up.Payload.MTimeNS,
		}); err != nil {
			return err
		}
	}
	if d.isRootStaged(rootID) {
		d.clearStagedRoot(rootID)
	}
	d.addWatcherRoot(rootID, root.TargetAbsPath)
	return nil
}

func (d *Daemon) applyCurrentRemoteHead(ctx context.Context, seq int64) error {
	if seq <= 0 {
		return nil
	}
	resp, apiErr, err := d.conn.FetchEvents(ctx, seq-1, 8)
	if err != nil {
		if apiErr != nil && apiErr.Code == "resync_required" {
			return d.bootstrap(ctx)
		}
		return err
	}
	for _, event := range resp.Events {
		if event.Seq == seq {
			return d.applyRemoteEvent(ctx, event)
		}
	}
	return fmt.Errorf("remote head %d not found", seq)
}

func (d *Daemon) recordScanWarnings(root state.Root, warnings []string) error {
	prefix := "scanner:" + root.RootID + ":"
	if err := d.stateDB.ClearWarningsWithPrefix(prefix); err != nil {
		return err
	}
	for i, warning := range warnings {
		message := fmt.Sprintf("%s: %s", root.HomeRelPath, warning)
		if err := d.stateDB.UpsertWarning(fmt.Sprintf("%s%d", prefix, i), message, time.Now().UTC()); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) handleDeletedRoot(ctx context.Context, root state.Root, queueRetryable bool) error {
	if root.State != protocol.RootStateActive {
		return nil
	}
	if _, err := d.submitEvent(ctx, root.RootID, "", "", protocol.EventRootRemove, nil, protocol.RootRemovePayload{RootID: root.RootID}, nil); err != nil {
		if isBenignRootRemoveError(err) {
			return d.markRootRemoved(root)
		}
		if queueRetryable && isRetryableSyncError(err) {
			if err := d.markRootRemoved(root); err != nil {
				return err
			}
			if err := d.stateDB.QueueRootRemove(root.RootID, time.Now().UTC()); err != nil {
				return err
			}
			_ = d.stateDB.SetConnectionStateWithKind(protocol.ConnectionDegraded, lifecycleKind(err, protocol.IssueTransport), err.Error())
			return nil
		}
		return err
	}
	return d.markRootRemoved(root)
}

func (d *Daemon) markRootRemoved(root state.Root) error {
	d.watcher.RemoveRoot(root.RootID)
	d.clearStagedRoot(root.RootID)
	if err := d.stateDB.ClearWarningsWithPrefix("watcher:" + root.RootID); err != nil {
		return err
	}
	if err := d.stateDB.ClearWarningsWithPrefix("scanner:" + root.RootID + ":"); err != nil {
		return err
	}
	if err := d.stateDB.SetRootState(root.RootID, protocol.RootStateRemoved); err != nil {
		return err
	}
	return d.stateDB.DeleteRootState(root.RootID)
}

func rootPathMissing(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, os.ErrNotExist)
}

func (d *Daemon) addWatcherRoot(rootID, target string) {
	if err := d.watcher.AddRoot(rootID, target); err != nil {
		message := fmt.Sprintf("watcher could not monitor %s: %v", target, err)
		_ = d.stateDB.UpsertWarning("watcher:"+rootID, message, time.Now().UTC())
		return
	}
	_ = d.stateDB.ClearWarningsWithPrefix("watcher:" + rootID)
}

func warningIssueKind(key string) protocol.DaemonIssueKind {
	switch {
	case strings.HasPrefix(key, "service:install"):
		return protocol.IssueServiceInstall
	case strings.HasPrefix(key, "service:disable"):
		return protocol.IssueServiceDisable
	case strings.HasPrefix(key, "watcher:"):
		return protocol.IssueWatcher
	case strings.HasPrefix(key, "scanner:"):
		return protocol.IssueScanner
	default:
		return ""
	}
}

func (d *Daemon) ensureSession(ctx context.Context) error {
	if err := validateServerURL(d.cfg.ServerURL); err != nil {
		return err
	}
	st, err := d.stateDB.LoadWorkspaceState()
	if err != nil {
		return err
	}
	if st.SessionToken != "" && !st.SessionExpiresAt.IsZero() && time.Now().UTC().Before(st.SessionExpiresAt.Add(-5*time.Minute)) {
		d.conn = connector.New(d.cfg.ServerURL).WithToken(st.SessionToken)
		return nil
	}
	session, err := authenticateSession(ctx, sessionHandshake{
		ServerURL:       d.cfg.ServerURL,
		WorkspaceID:     d.cfg.WorkspaceID,
		DeviceID:        d.cfg.DeviceID,
		DeviceName:      d.cfg.DeviceName,
		Keys:            d.keys,
		CreateIfMissing: false,
	})
	if err != nil {
		return err
	}
	d.conn = session.Client
	return d.stateDB.SaveWorkspaceState(state.WorkspaceState{
		ServerURL:        d.cfg.ServerURL,
		WorkspaceID:      d.cfg.WorkspaceID,
		SessionToken:     session.Token,
		SessionExpiresAt: session.ExpiresAt,
		LastServerSeq:    st.LastServerSeq,
		ConnectionState:  protocol.ConnectionAuthenticating,
		LastError:        "",
	})
}

func (d *Daemon) startReconnectLoopLocked() {
	if d.reconnectCancel != nil {
		return
	}
	baseCtx := d.runCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithCancel(baseCtx)
	d.reconnectCancel = cancel
	d.reconnectLoopID++
	loopID := d.reconnectLoopID
	go func() {
		defer func() {
			d.mu.Lock()
			if d.reconnectLoopID == loopID {
				d.reconnectCancel = nil
			}
			d.mu.Unlock()
		}()
		d.reconnectLoop(ctx)
	}()
}

func (d *Daemon) blockedRetryLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.syncMu.Lock()
			if err := d.retryBlockedRoots(ctx); err != nil && !errors.Is(err, context.Canceled) {
				d.logger.Printf("retry blocked roots: %v", err)
			}
			d.syncMu.Unlock()
		}
	}
}

func (d *Daemon) retryBlockedRoots(ctx context.Context) error {
	roots, err := d.stateDB.ListRoots()
	if err != nil {
		return err
	}
	var blocked []state.Root
	for _, root := range roots {
		if root.State == protocol.RootStateBlockedNonEmpty {
			blocked = append(blocked, root)
		}
	}
	if len(blocked) == 0 {
		return nil
	}
	d.mu.Lock()
	conn := d.conn
	d.mu.Unlock()
	if conn == nil || d.keys == nil || d.cfg.WorkspaceID == "" {
		return nil
	}
	resp, err := conn.Bootstrap(ctx)
	if err != nil {
		return err
	}
	remoteRoots := make(map[string]protocol.BootstrapRoot, len(resp.Roots))
	for _, root := range resp.Roots {
		remoteRoots[root.RootID] = root
	}
	for _, localRoot := range blocked {
		remoteRoot, ok := remoteRoots[localRoot.RootID]
		if !ok {
			continue
		}
		if _, err := d.ensureRootFromDescriptor(remoteRoot); err != nil {
			var integrityErr *applier.IntegrityError
			if errors.As(err, &integrityErr) {
				d.logger.Printf("rejected blocked-root descriptor %s: %v", remoteRoot.RootID, integrityErr)
				continue
			}
			return err
		}
		updated, err := d.stateDB.RootByID(localRoot.RootID)
		if err != nil {
			return err
		}
		if updated.State != protocol.RootStateActive {
			continue
		}
		if remoteRoot.LatestSnapshotObjectID != "" && remoteRoot.LatestSnapshotSeq > 0 {
			if err := d.applySnapshot(ctx, *updated, remoteRoot.LatestSnapshotObjectID, remoteRoot.LatestSnapshotSeq, d.isRootStaged(updated.RootID)); err != nil {
				var integrityErr *applier.IntegrityError
				if errors.As(err, &integrityErr) {
					d.logger.Printf("rejected blocked-root snapshot %s: %v", remoteRoot.RootID, integrityErr)
					continue
				}
				return err
			}
		}
		if err := d.catchUpRootFrom(ctx, updated.RootID, remoteRoot.LatestSnapshotSeq); err != nil {
			return err
		}
		if !d.isRootStaged(updated.RootID) {
			d.addWatcherRoot(updated.RootID, updated.TargetAbsPath)
		}
	}
	return nil
}

func (d *Daemon) catchUpRootFrom(ctx context.Context, rootID string, afterSeq int64) error {
	cursor := afterSeq
	for {
		resp, _, err := d.conn.FetchEvents(ctx, cursor, 1000)
		if err != nil {
			return err
		}
		if len(resp.Events) == 0 {
			return nil
		}
		for _, event := range resp.Events {
			if event.RootID == rootID && event.Seq > afterSeq {
				if err := d.applyRemoteEvent(ctx, event); err != nil {
					return err
				}
			}
			if event.Seq > cursor {
				cursor = event.Seq
			}
		}
		if cursor >= resp.CurrentSeq {
			return nil
		}
	}
}

func (d *Daemon) reconcileActiveRoots(ctx context.Context) error {
	roots, err := d.stateDB.ListRoots()
	if err != nil {
		return err
	}
	for _, root := range roots {
		if root.State != protocol.RootStateActive {
			continue
		}
		if err := d.rescanRoot(ctx, root.RootID); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) flushPendingOps(ctx context.Context) error {
	ops, err := d.stateDB.ListPendingOpsReady(time.Now().UTC())
	if err != nil {
		return err
	}
	for _, op := range ops {
		switch op.OpType {
		case "rescan_root":
			err := d.rescanRootWithRetry(ctx, op.RootID, true, false)
			var conflict *PathConflictError
			if errors.As(err, &conflict) {
				if headErr := d.applyCurrentRemoteHead(ctx, conflict.CurrentSeq); headErr != nil {
					err = headErr
				} else {
					err = d.rescanRootWithRetry(ctx, op.RootID, true, false)
				}
			}
			if err != nil {
				_ = d.stateDB.BumpPendingOpRetry(op.OpID, err.Error(), time.Now().UTC())
				return err
			}
			if err := d.stateDB.DeletePendingOp(op.OpID); err != nil {
				return err
			}
		case "root_remove":
			_, err := d.submitEvent(ctx, op.RootID, "", "", protocol.EventRootRemove, nil, protocol.RootRemovePayload{RootID: op.RootID}, nil)
			if err != nil && !isBenignRootRemoveError(err) {
				_ = d.stateDB.BumpPendingOpRetry(op.OpID, err.Error(), time.Now().UTC())
				return err
			}
			if err := d.stateDB.DeletePendingOp(op.OpID); err != nil {
				return err
			}
		default:
			if err := d.stateDB.DeletePendingOp(op.OpID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Daemon) queuePendingRootRescan(rootID string, cause error) error {
	if err := d.stateDB.QueueRootRescan(rootID, time.Now().UTC()); err != nil {
		return err
	}
	if cause != nil {
		_ = d.stateDB.SetConnectionStateWithKind(protocol.ConnectionDegraded, lifecycleKind(cause, protocol.IssueTransport), cause.Error())
	}
	return nil
}

func (d *Daemon) markStagedRoot(rootID string) {
	d.stageMu.Lock()
	defer d.stageMu.Unlock()
	d.stagedRoots[rootID] = struct{}{}
}

func (d *Daemon) clearStagedRoot(rootID string) {
	d.stageMu.Lock()
	defer d.stageMu.Unlock()
	delete(d.stagedRoots, rootID)
}

func (d *Daemon) isRootStaged(rootID string) bool {
	d.stageMu.RLock()
	defer d.stageMu.RUnlock()
	_, ok := d.stagedRoots[rootID]
	return ok
}

func (d *Daemon) setStreamingLive(v bool) {
	d.stageMu.Lock()
	defer d.stageMu.Unlock()
	d.streamingLive = v
}

func (d *Daemon) isStreamingLive() bool {
	d.stageMu.RLock()
	defer d.stageMu.RUnlock()
	return d.streamingLive
}

func (d *Daemon) enqueueRootRescan(rootID string) {
	select {
	case d.changeCh <- watcher.Change{RootID: rootID}:
	default:
	}
}

func (d *Daemon) shouldSuppressIgnoredDelete(rootID, relPath string, now time.Time) (bool, error) {
	rule, ignored, err := d.stateDB.IgnoreRule(rootID, relPath, now)
	if err != nil || !ignored {
		return false, err
	}
	return rule.Deleted, nil
}

func (d *Daemon) shouldSuppressIgnoredEntry(rootID string, item scanner.Entry, now time.Time) (bool, error) {
	rule, ignored, err := d.stateDB.IgnoreRule(rootID, item.RelPath, now)
	if err != nil || !ignored {
		return false, err
	}
	if rule.Deleted || rule.Kind != item.Kind || rule.Mode != item.Mode || rule.MTimeNS != item.MTimeNS {
		return false, nil
	}
	if item.Kind == protocol.RootKindFile && rule.ContentSHA256 != item.ContentSHA256 {
		return false, nil
	}
	return true, nil
}

func normalizeRescanHint(relPath string) string {
	relPath = filepath.ToSlash(filepath.Clean(filepath.FromSlash(relPath)))
	if relPath == "." || relPath == "/" {
		return ""
	}
	return strings.TrimPrefix(relPath, "./")
}

func syncOrder(entries []scanner.Entry) []scanner.Entry {
	ordered := append([]scanner.Entry(nil), entries...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Kind != ordered[j].Kind {
			return ordered[i].Kind == protocol.RootKindFile
		}
		return ordered[i].RelPath < ordered[j].RelPath
	})
	return ordered
}

func mergeRescanHints(current, next string) string {
	current = normalizeRescanHint(current)
	next = normalizeRescanHint(next)
	if current == "" || next == "" {
		return ""
	}
	currentParts := strings.Split(current, "/")
	nextParts := strings.Split(next, "/")
	limit := len(currentParts)
	if len(nextParts) < limit {
		limit = len(nextParts)
	}
	var shared []string
	for i := 0; i < limit; i++ {
		if currentParts[i] != nextParts[i] {
			break
		}
		shared = append(shared, currentParts[i])
	}
	return strings.Join(shared, "/")
}

func filterEntriesByHint(entries map[string]state.Entry, relPathHint string) map[string]state.Entry {
	relPathHint = normalizeRescanHint(relPathHint)
	if relPathHint == "" {
		return entries
	}
	filtered := make(map[string]state.Entry)
	for relPath, entry := range entries {
		if entryInHint(relPath, relPathHint) {
			filtered[relPath] = entry
		}
	}
	return filtered
}

func entryInHint(relPath, relPathHint string) bool {
	relPath = normalizeRescanHint(relPath)
	relPathHint = normalizeRescanHint(relPathHint)
	if relPathHint == "" {
		return true
	}
	return relPath == relPathHint || strings.HasPrefix(relPath, relPathHint+"/")
}

func jitter(base time.Duration) time.Duration {
	if base <= 0 {
		return time.Second
	}
	random := time.Duration(uuid.New().ID()%uint32(base/time.Millisecond+1)) * time.Millisecond
	if random <= 0 {
		return time.Millisecond
	}
	return random
}

func isRetryableSyncError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, token := range []string{
		"connection refused",
		"connection reset",
		"connection closed",
		"no such host",
		"timeout",
		"temporary failure",
		"unexpected eof",
		"eof",
		"use of closed network connection",
		"broken pipe",
		"no route to host",
		"network is unreachable",
		"session expired",
		"unauthorized",
	} {
		if strings.Contains(message, token) {
			return true
		}
	}
	return false
}

func isBenignRootRemoveError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "root already removed") || strings.Contains(message, "unknown root")
}

func validateServerURL(serverURL string) error {
	u, err := url.Parse(serverURL)
	if err != nil {
		return fmt.Errorf("parse server URL: %w", err)
	}
	if u.Host == "" {
		return fmt.Errorf("server URL must include a host")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if allowInsecureTransport() {
			return nil
		}
		return fmt.Errorf("insecure http transport is disabled; set %s=true only for local development", insecureTransportEnv)
	default:
		return fmt.Errorf("unsupported server URL scheme %q", u.Scheme)
	}
}

func allowInsecureTransport() bool {
	return strings.EqualFold(os.Getenv(insecureTransportEnv), "true")
}

func (d *Daemon) logRemoteIntegrityError(event protocol.EventRecord, err error) {
	d.logger.Printf("rejected remote %s for root %s seq %d: %v", event.EventType, event.RootID, event.Seq, err)
}

func ptrInt64(v int64) *int64 {
	return &v
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	var out []string
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
