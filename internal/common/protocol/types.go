package protocol

import "time"

const VersionHeader = "X-Syna-Protocol"

const (
	MaxFileChunkPlainSize = 4 << 20
	MaxSnapshotPlainSize  = 16 << 20
	MaxWSMessageBytes     = 1 << 20
)

type ConnectionState string

const (
	ConnectionNeedsBootstrap  ConnectionState = "needs_bootstrap"
	ConnectionDisconnected    ConnectionState = "disconnected"
	ConnectionAuthenticating  ConnectionState = "authenticating"
	ConnectionCatchingUp      ConnectionState = "catching_up"
	ConnectionLive            ConnectionState = "live"
	ConnectionBlockedNonEmpty ConnectionState = "blocked_nonempty"
	ConnectionDegraded        ConnectionState = "degraded"
)

type DaemonIssueKind string

const (
	IssueServiceInstall DaemonIssueKind = "service_install"
	IssueServiceDisable DaemonIssueKind = "service_disable"
	IssueSocket         DaemonIssueKind = "socket"
	IssueAuth           DaemonIssueKind = "auth"
	IssueBootstrap      DaemonIssueKind = "bootstrap"
	IssueWatcher        DaemonIssueKind = "watcher"
	IssueTransport      DaemonIssueKind = "transport"
	IssueScanner        DaemonIssueKind = "scanner"
)

type EventType string

const (
	EventRootAdd    EventType = "root_add"
	EventRootRemove EventType = "root_remove"
	EventDirPut     EventType = "dir_put"
	EventFilePut    EventType = "file_put"
	EventDelete     EventType = "delete"
)

type RootKind string

const (
	RootKindFile RootKind = "file"
	RootKindDir  RootKind = "dir"
)

type RootState string

const (
	RootStateActive          RootState = "active"
	RootStateBlockedNonEmpty RootState = "blocked_nonempty"
	RootStateRemoved         RootState = "removed"
)

type SessionStartRequest struct {
	WorkspaceID     string `json:"workspace_id"`
	DeviceID        string `json:"device_id"`
	DeviceName      string `json:"device_name"`
	ClientNonce     string `json:"client_nonce"`
	CreateIfMissing bool   `json:"create_if_missing"`
	WorkspacePubKey string `json:"workspace_pubkey,omitempty"`
}

type SessionStartResponse struct {
	WorkspaceExists bool      `json:"workspace_exists"`
	Created         bool      `json:"created"`
	ServerNonce     string    `json:"server_nonce"`
	ServerTime      time.Time `json:"server_time"`
	ProtocolVersion int       `json:"protocol_version"`
}

type SessionFinishRequest struct {
	WorkspaceID string `json:"workspace_id"`
	DeviceID    string `json:"device_id"`
	ClientNonce string `json:"client_nonce"`
	ServerNonce string `json:"server_nonce"`
	Signature   string `json:"signature"`
}

type SessionFinishResponse struct {
	SessionToken string    `json:"session_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	WorkspaceID  string    `json:"workspace_id"`
	CurrentSeq   int64     `json:"current_seq"`
}

type BootstrapRoot struct {
	RootID                 string   `json:"root_id"`
	Kind                   RootKind `json:"kind"`
	DescriptorBlob         string   `json:"descriptor_blob"`
	CreatedSeq             int64    `json:"created_seq"`
	RemovedSeq             *int64   `json:"removed_seq"`
	LatestSnapshotObjectID string   `json:"latest_snapshot_object_id,omitempty"`
	LatestSnapshotSeq      int64    `json:"latest_snapshot_seq,omitempty"`
}

type BootstrapResponse struct {
	WorkspaceID       string          `json:"workspace_id"`
	CurrentSeq        int64           `json:"current_seq"`
	BootstrapAfterSeq int64           `json:"bootstrap_after_seq"`
	Roots             []BootstrapRoot `json:"roots"`
}

type EventSubmitRequest struct {
	RootID      string    `json:"root_id"`
	PathID      string    `json:"path_id,omitempty"`
	RootKind    RootKind  `json:"root_kind,omitempty"`
	EventType   EventType `json:"event_type"`
	BaseSeq     *int64    `json:"base_seq,omitempty"`
	PayloadBlob string    `json:"payload_blob"`
	ObjectRefs  []string  `json:"object_refs"`
}

type EventSubmitResponse struct {
	AcceptedSeq  int64 `json:"accepted_seq"`
	WorkspaceSeq int64 `json:"workspace_seq"`
}

type ErrorResponse struct {
	Code             string `json:"code"`
	Message          string `json:"message,omitempty"`
	CurrentSeq       int64  `json:"current_seq,omitempty"`
	RetainedFloorSeq int64  `json:"retained_floor_seq,omitempty"`
}

type EventRecord struct {
	Seq            int64     `json:"seq"`
	RootID         string    `json:"root_id"`
	PathID         *string   `json:"path_id"`
	EventType      EventType `json:"event_type"`
	BaseSeq        *int64    `json:"base_seq"`
	AuthorDeviceID string    `json:"author_device_id"`
	PayloadBlob    string    `json:"payload_blob"`
	ObjectRefs     []string  `json:"object_refs"`
	CreatedAt      time.Time `json:"created_at"`
}

type EventFetchResponse struct {
	Events     []EventRecord `json:"events"`
	CurrentSeq int64         `json:"current_seq"`
}

type SnapshotSubmitRequest struct {
	RootID     string   `json:"root_id"`
	BaseSeq    int64    `json:"base_seq"`
	ObjectID   string   `json:"object_id"`
	ObjectRefs []string `json:"object_refs"`
}

type SnapshotSubmitResponse struct {
	RootID   string `json:"root_id"`
	BaseSeq  int64  `json:"base_seq"`
	ObjectID string `json:"object_id"`
}

type ChunkRef struct {
	ObjectID  string `json:"object_id"`
	PlainSize int64  `json:"plain_size"`
}

type RootAddPayload struct {
	RootID      string   `json:"root_id"`
	Kind        RootKind `json:"kind"`
	HomeRelPath string   `json:"home_rel_path"`
}

type RootRemovePayload struct {
	RootID string `json:"root_id"`
}

type DirPutPayload struct {
	Path    string `json:"path"`
	Mode    int64  `json:"mode"`
	MTimeNS int64  `json:"mtime_ns"`
}

type FilePutPayload struct {
	Path          string     `json:"path"`
	Mode          int64      `json:"mode"`
	MTimeNS       int64      `json:"mtime_ns"`
	SizeBytes     int64      `json:"size_bytes"`
	ContentSHA256 string     `json:"content_sha256"`
	Chunks        []ChunkRef `json:"chunks"`
}

type DeletePayload struct {
	Path string `json:"path"`
}

type SnapshotEntry struct {
	Path          string     `json:"path"`
	Kind          RootKind   `json:"kind"`
	Mode          int64      `json:"mode"`
	MTimeNS       int64      `json:"mtime_ns"`
	SizeBytes     int64      `json:"size_bytes,omitempty"`
	ContentSHA256 string     `json:"content_sha256,omitempty"`
	Chunks        []ChunkRef `json:"chunks,omitempty"`
}

type SnapshotPayload struct {
	RootID      string          `json:"root_id"`
	Kind        RootKind        `json:"kind"`
	HomeRelPath string          `json:"home_rel_path"`
	BaseSeq     int64           `json:"base_seq"`
	Entries     []SnapshotEntry `json:"entries"`
}

type WorkspaceStatus struct {
	ServerURL     string          `json:"server_url,omitempty"`
	WorkspaceID   string          `json:"workspace_id,omitempty"`
	Connection    ConnectionState `json:"connection_state"`
	LastServerSeq int64           `json:"last_server_seq"`
	PendingOps    int64           `json:"pending_ops"`
	LastErrorKind DaemonIssueKind `json:"last_error_kind,omitempty"`
	LastError     string          `json:"last_error,omitempty"`
	Issues        []DaemonIssue   `json:"issues,omitempty"`
	Warnings      []string        `json:"warnings,omitempty"`
	TrackedRoots  []RootStatus    `json:"tracked_roots,omitempty"`
}

type DaemonIssue struct {
	Kind    DaemonIssueKind `json:"kind"`
	Message string          `json:"message"`
}

type RootStatus struct {
	RootID        string    `json:"root_id"`
	Kind          RootKind  `json:"kind"`
	HomeRelPath   string    `json:"home_rel_path"`
	TargetAbsPath string    `json:"target_abs_path"`
	State         RootState `json:"state"`
}

type RPCRequest struct {
	Method string `json:"method"`
	Body   any    `json:"body,omitempty"`
}

type RPCResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Body  any    `json:"body,omitempty"`
}

type WSMessage struct {
	Type       string       `json:"type"`
	Event      *EventRecord `json:"event,omitempty"`
	ServerTime time.Time    `json:"server_time,omitempty"`
}
