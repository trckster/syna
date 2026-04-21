# Syna Client Design

## Binary Layout

Syna v1 uses one Linux binary named `syna`.

It supports two modes:

- user CLI mode
- background daemon mode

There is no separate third client-daemon binary. `syna daemon` is the long-lived daemon mode of the `syna` binary.

When `daemon_auto_start=true`, the daemon is managed by a `systemd --user` service and the CLI starts that service when the Unix socket is absent.
If user systemd is unavailable, automatic startup fails instead of launching a private `syna daemon` process.

## Daemon Lifecycle On Linux

Syna v1 uses a per-user `systemd --user` unit named `syna.service`.

Rules:

- `ExecStart` must run `syna daemon`
- if `daemon_auto_start=true`, the CLI must install or refresh `~/.config/systemd/user/syna.service` before starting the daemon
- when starting the daemon, the CLI must run `systemctl --user daemon-reload` and `systemctl --user start syna.service`
- after a successful `syna connect <server-url>`, the daemon must run `systemctl --user enable --now syna.service`
- later CLI invocations must first try `~/.local/state/syna/agent.sock`
- if the socket is absent and `daemon_auto_start=true`, the CLI must use user systemd to start `syna.service` and wait briefly for the socket
- if user systemd is unavailable while `daemon_auto_start=true`, the CLI must fail clearly and must not launch `syna daemon` directly
- if the socket is absent and `daemon_auto_start=false`, the CLI must fail with a clear message telling the user to start `syna daemon` or the user service manually

Default reboot behavior is "restart for that user at next login". Systems that require sync before login may enable `loginctl enable-linger <user>` separately.

## CLI To Daemon Communication

The CLI is a short-lived front end. It never talks to the server directly.

Transport:

- Unix domain socket at `~/.local/state/syna/agent.sock`
- same-user only
- state directory permissions `0700`
- socket permissions `0600`

RPC responsibilities:

- `syna connect <server-url>` asks the daemon to connect or create the workspace
- `syna disconnect` asks the daemon to stop syncing this device and clear local workspace state
- `syna key show` reads the locally stored recovery key from `keyring.json` without contacting the daemon or server
- `syna add <path>` asks the daemon to validate, scan, and add a root
- `syna rm <path>` asks the daemon to stop watching and submit `root_remove`
- `syna status` asks the daemon for a snapshot of local and remote sync state
- `syna help`, `syna -h`, and `syna --help` print CLI usage without contacting the daemon

The daemon owns all writes to `config.json`, `keyring.json`, and `client.db`. The CLI only renders prompts and command output, except that `syna key show` may read `keyring.json` directly.

## Daemon Startup And Recovery

Every daemon start, including reboot recovery, must:

1. load config, keyring, and local DB
2. create `daemon.pid`
3. bind `agent.sock`
4. restore tracked roots and pending operations from `client.db`
5. authenticate and re-establish the server session
6. catch up from `/v1/events`, or fall back to `/v1/bootstrap` if the cursor is too old
7. reconcile each active root against the local filesystem so changes made while the daemon was stopped are detected
8. install recursive watches
9. open the live WebSocket feed

The daemon must not report a root as healthy live sync until both remote catch-up and startup reconciliation have completed for that root.

If the daemon discovers that the same path changed both remotely and locally while it was down, it must preserve both versions using the normal conflict filename rules rather than overwriting either side silently.

## Daemon To Server Communication

Only the daemon talks to the backend.

Control plane:

- `POST /v1/session/start`
- `POST /v1/session/finish`
- `GET /v1/bootstrap`
- `GET /v1/events`
- `POST /v1/events`

Object plane:

- `PUT /v1/objects/{object_id}`
- `GET /v1/objects/{object_id}`

Live plane:

- `GET /v1/ws`

Rules:

- use HTTPS for all request/response traffic
- keep exactly one active authenticated session per running daemon
- renew the bearer token before expiry
- perform backlog catch-up over HTTP before trusting the live WebSocket stream
- if the socket drops, reconnect and catch up with `GET /v1/events`

## CLI Contract

### `syna connect <server-url>`

Connect to a server and either:

- create a new workspace on first use for that server
- prompt once for a recovery key on additional devices

The command stores:

- active server URL in config
- workspace key in `keyring.json`
- session state in the local DB

If this is the first client for that server, print the generated recovery key to stdout as `Your secret key: syna1-...`, then explain that the key lets other devices join and should not be shared.

If the user provides an existing recovery key, print `Connection established!` after the connection succeeds.

If the installation is already connected to a different server, the command must fail with a clear message telling the user to run `syna disconnect` first.

### `syna key show`

Print the locally stored workspace recovery key to stdout.

Behavior:

- read `~/.config/syna/keyring.json` directly
- print only the `workspace_key` value plus a trailing newline
- do not contact the daemon
- do not contact the server
- fail clearly if no workspace key is stored
- reject unsupported forms such as `syna key`, `syna key list`, or extra arguments with usage and exit code 2

### `syna help`, `syna -h`, `syna --help`

Print the CLI usage summary and exit.

Behavior:

- list every supported user-facing command
- include one-line descriptions for `connect`, `disconnect`, `key show`, `add`, `rm`, and `status`
- do not require local daemon availability
- do not contact the server

### `syna disconnect`

Disconnect this installation from its active workspace.

Behavior:

- stop local watching and close the active server connection
- drop pending operations for that workspace
- clear the stored workspace key, server URL, workspace ID, session token, roots, entries, ignore windows, and pending-op state
- disable and stop `syna.service`
- leave local synced files on disk untouched
- do not modify the server workspace or other clients

A later reconnect is treated as a fresh device join, so existing local targets are checked with the normal bootstrap blocking rules.

### `syna add <path>`

Add a file or directory root to the active workspace.

Checks:

- must already be connected to a server
- path must exist
- path must be under `$HOME`
- path must not overlap an existing root
- root itself must be a regular file or directory
- unsupported descendants are skipped and surfaced as warnings

### `syna rm <path>`

Remove a tracked root from the active workspace.

Behavior:

- stop watching it locally
- submit a `root_remove` event
- keep local files on disk untouched
- other clients also stop syncing that root and keep local files untouched
- the root's encrypted server-side history becomes eligible for deletion by retention and GC
- adding the same path again later must behave like a fresh first-time add

### `syna status`

Print:

- active server URL
- connection state
- last server sequence
- number of pending operations
- tracked roots and their states
- last error if any
- unsupported file warnings

## Local Files

Use:

```text
~/.config/syna/config.json
~/.config/syna/keyring.json
~/.local/state/syna/client.db
~/.local/state/syna/agent.sock
~/.local/state/syna/daemon.pid
```

## One Workspace Per Installation

The client handles one active workspace in v1.

Reason:

- it keeps CLI semantics simple
- it avoids cross-workspace watcher and state complexity
- it matches the product input better than adding account or profile management

If the user wants to switch servers, they must run `syna disconnect` first.

## Internal Daemon Modules

- `agentrpc`: Unix socket RPC server for CLI commands
- `configstore`: config and keyring loading
- `state`: SQLite access
- `connector`: server auth, HTTP, WebSocket, reconnect
- `scanner`: full tree scans and subtree rescans
- `watcher`: recursive inotify management
- `uploader`: chunk encryption and object upload
- `applier`: remote event application
- `snapshotter`: periodic snapshot creation

## Connection State Machine

States:

- `disconnected`
- `authenticating`
- `catching_up`
- `live`
- `degraded`

Rules:

- only `live` means the websocket is healthy and catch-up is complete
- `degraded` means local watching continues and ops queue locally, but the server connection is unhealthy

## Add Root Algorithm

1. Canonicalize the requested path.
2. Convert it to `home_rel_path`.
3. Compute `root_id`.
4. Insert a provisional local root record.
5. Perform a full scan.
6. Build the root entry event for `path=""` as `dir_put` for a directory root or `file_put` for a file root.
7. For each descendant directory entry, queue `dir_put`.
8. For each descendant regular file:
   - compute plaintext SHA-256
   - split into 4 MiB chunks
   - encrypt each chunk
   - upload chunk objects
   - queue `file_put`
9. Submit `root_add`.
10. Submit the root entry event for `path=""`.
11. Submit all remaining queued path events in deterministic path order.
12. Upload the first snapshot after initial sync succeeds, including the deduplicated chunk `object_refs` for that snapshot.
13. Activate recursive watchers.

Deterministic initial path order:

- root entry `path=""` first
- then directories lexicographically
- then files lexicographically

## Bootstrap Algorithm

For each root from `/v1/bootstrap`:

1. Decrypt the root descriptor.
2. Resolve `target_abs_path = $HOME + "/" + home_rel_path`.
3. If local state already has this `root_id` in state `removed` and `target_abs_path` matches, mark it as a reactivation candidate.
4. If the root is not a reactivation candidate and does not exist locally:
   - create required parent directories
5. If the root is not a reactivation candidate, is a directory, and exists but is non-empty:
   - mark the root as `blocked_nonempty`
   - do not apply the root
6. If the root is not a reactivation candidate, is a file, and already exists:
   - mark the root as `blocked_nonempty`
   - do not apply the root
7. If not blocked and a snapshot exists:
   - download the latest snapshot object
   - if this is not a reactivation candidate, decrypt and materialize it directly
   - if this is a reactivation candidate, decrypt it into a temporary staging tree or in-memory index instead of overwriting the existing local target
8. After all roots have either been materialized or marked blocked, fetch retained events once with `GET /v1/events?after_seq=<bootstrap_after_seq>`.
9. Apply those events in ascending sequence order. For blocked roots, still apply root-lifecycle events but skip content events until the root becomes unblocked.
10. For reactivated roots that were not blocked, reconcile the staged remote state and retained events against the existing local tree before declaring them healthy so leftover local bytes from the removed incarnation are preserved through normal pending-op or conflict rules.
11. Start watchers for active unblocked roots.

Blocked roots are retried automatically on daemon restart and every 30 seconds while the target remains blocked.

When a blocked root becomes unblocked, the daemon must re-bootstrap that root from the latest descriptor, snapshot, and retained events instead of trying to replay the skipped backlog blindly.

## Watcher Strategy

Use Linux inotify through `fsnotify`.

Rules:

- install watches recursively for all directories in active roots
- when a new directory appears, add a watch immediately
- debounce bursts for 500 milliseconds
- on rename or move, rescan the nearest existing ancestor and emit delete plus put semantics
- on chmod-only changes, rescan metadata and emit `dir_put` or `file_put` if mode changed

## Rescan Rules

The watcher is a hint, not the source of truth.

When a watched path changes:

- stat the path if it still exists
- compare with the local `entries` index
- if needed, hash the file content again
- produce canonical events from the index diff

## Remote Apply Rules

Remote events are applied serially per root in accepted sequence order.

### Root add apply

- create or reactivate the local root record from the descriptor
- if this is a reactivation of a locally removed root with the same `target_abs_path`, do not apply the normal non-empty bootstrap block
- if this is not a reactivation and the local target violates the bootstrap blocking rules, mark the root `blocked_nonempty`
- for a reactivated root, stage remote snapshot and event content until a reconciliation scan finishes so preserved local files are treated as local changes rather than silently discarded
- if the root is not blocked, accept following `dir_put` and `file_put` events for that root
- for reactivated roots, "accept" means queue into the staged remote view until reconciliation completes

### Root remove apply

- stop watchers for that root
- delete local `entries`, `pending_ops`, and ignore windows for that root
- mark the root `removed`
- keep local files on disk untouched

### Directory apply

- ensure parent exists
- create the directory if missing
- set mode
- set mtime

### File apply

1. download all referenced objects
2. decrypt chunks
3. write to a temp file in the target directory
4. `fsync` the temp file
5. atomically rename over the final target
6. set mode
7. set mtime
8. record an ignore window for that path

### Delete apply

- remove file if it exists
- remove directory tree if it exists and is fully managed by that root

The client must never delete paths outside a tracked root.

## Self-change Suppression

Applying remote changes will itself trigger inotify.

Use `ignore_events` with:

- `root_id`
- `rel_path`
- expiration time = now + 2 seconds

If a watcher event lands within that window, rescan but suppress upload unless content actually differs from the indexed state.

## Conflict Resolution

If `POST /v1/events` returns `409 path_head_mismatch`:

1. keep a copy of the local changed bytes in memory or a temp file
2. fetch and apply the current remote head for the original path
3. create a conflict filename locally
4. write the losing version to that conflict path
5. upload the conflict path as a new `file_put`

Conflict filename format:

```text
<stem>.syna-conflict-<device-short>-<YYYYMMDDTHHMMSSZ><ext>
```

Example:

```text
notes.syna-conflict-laptop-20260417T123455Z.md
```

## Unsupported Inputs

The client must refuse or ignore:

- symlinks as roots
- symlinks inside tracked trees
- sockets inside tracked trees
- fifos inside tracked trees
- device nodes inside tracked trees

These must appear in `syna status` output as warnings, not silently disappear.

## Disconnect And Retry

If the network is down:

- keep local watchers active
- keep generating local pending operations
- retry session creation with exponential backoff
- on reconnect, upload pending operations in creation order after event catch-up

If incremental catch-up returns `410 resync_required`:

- discard the old incremental cursor
- run a fresh `/v1/bootstrap`
- rebuild local state from snapshots plus newer events
- keep any still-blocked paths in `blocked_nonempty`

Backoff:

- start: 1 second
- cap: 60 seconds
- full jitter

Intentional local disconnect is different from a network failure: after `syna disconnect`, the daemon must not retry or reconnect until the user explicitly runs `syna connect <server-url>` again.
