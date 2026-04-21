# Syna Architecture

## Runtime Components

### 1. Client binary

One Go binary named `syna` contains:

- CLI command handling
- local background agent
- local watcher engine
- local sync engine
- local encrypted object uploader/downloader
- local state database access

The local daemon is not a separate binary. It is the long-lived `syna daemon` mode of the `syna` executable.

### 2. Backend service

One Go binary named `syna-server` runs in Docker and contains:

- HTTPS API
- WebSocket live event hub
- workspace/session authentication
- SQLite metadata store
- encrypted object store on the mounted server volume
- snapshot and garbage collection workers

Syna v1 therefore ships two binaries, not three:

- `syna`
- `syna-server`

## Linux Client Lifecycle

Syna is a per-user Linux service, not a system-wide daemon shared across users.

The supported v1 lifecycle is:

- a `systemd --user` unit named `syna.service`
- `ExecStart` runs `syna daemon`
- when `daemon_auto_start=true`, the CLI must install or refresh that unit in `~/.config/systemd/user/` before starting the daemon
- when starting the daemon, the CLI must run `systemctl --user daemon-reload` and `systemctl --user start syna.service`
- after a successful `syna connect <server-url>`, the daemon must run `systemctl --user enable --now syna.service`
- later CLI invocations must connect to `~/.local/state/syna/agent.sock`
- if the socket is absent and `daemon_auto_start=true`, the CLI must use user systemd to start `syna.service` and wait for the socket to appear
- if user systemd is unavailable while `daemon_auto_start=true`, the CLI must fail with a clear fatal message and must not launch `syna daemon` directly
- if `daemon_auto_start=false`, the CLI must not enable the service automatically and should instead tell the user to start `syna daemon` or `systemctl --user start syna.service`

In the default Linux setup, "starts after reboot" means "starts for that user on the next login". Machines that must keep syncing before login may use `loginctl enable-linger <user>`, but that is an explicit operator choice rather than a client default.

## Trust Boundary

The server is not trusted with plaintext user data.

The client is trusted with:

- plaintext files on the local filesystem
- the workspace recovery key
- all encryption and decryption operations

The server is trusted only to:

- store encrypted blobs durably
- serialize event order
- relay live updates
- enforce optimistic concurrency on opaque path identifiers

## High-level Design

Syna uses:

- end-to-end encrypted file chunks
- encrypted root descriptors
- encrypted event payloads
- a workspace-global event sequence assigned by the server
- periodic encrypted root snapshots for fast bootstrap

The server never sees plaintext paths or plaintext file contents. It only sees opaque identifiers, object sizes, event types, timestamps, device IDs, and workspace IDs.

## Main Flows

## First Device Connect

1. The user runs `syna connect https://server.example.com`.
2. The client starts or contacts the local daemon.
3. No workspace key exists locally for that server, so the daemon generates a new workspace recovery key.
4. The daemon derives:
   - workspace ID
   - Ed25519 authentication keypair
   - path ID key
   - content encryption keys
5. The daemon creates the workspace on the server and prints the recovery key with a short warning not to share it.
6. The key is stored locally in plain text as required by product input.

## Additional Device Connect

1. The user runs `syna connect https://server.example.com`.
2. If the installation is already connected to a different server, the CLI rejects the command and requires `syna disconnect` first.
3. Otherwise, the daemon has no saved key for that server, so it prompts once for the workspace recovery key.
4. The daemon derives the same workspace ID and auth keypair.
5. The daemon authenticates to the existing workspace.
6. The daemon downloads encrypted root descriptors, the latest snapshots, and newer events.
7. Each root is restored under `$HOME/<stored-home-relative-path>` unless blocked by an existing local target that violates the bootstrap rules.

## Add Root

1. The user runs `syna add ~/.obsidian`.
2. The daemon canonicalizes the path and converts it to a home-relative root.
3. The daemon rejects the request if the root overlaps an existing root.
4. The daemon performs a full scan.
5. File contents are chunked, encrypted locally, and uploaded as objects.
6. The daemon sends a `root_add` event for that root.
7. The daemon submits the root entry event for `relative_path=""`, then the remaining directory and file events.
8. The daemon uploads an initial encrypted snapshot plus the chunk object references reachable from that snapshot.
9. The daemon begins watching the root recursively with inotify.

## Disconnect Device

1. The user runs `syna disconnect`.
2. The CLI contacts the local daemon.
3. The daemon stops watchers, closes the WebSocket, and drops any local pending operations for that workspace.
4. The daemon clears the local workspace key and workspace state for that installation.
5. The daemon disables and stops `syna.service`.
6. Local files stay on disk unchanged.

## Live Update

1. A file changes locally.
2. The daemon debounces the change and rescans the affected subtree.
3. New file chunks are encrypted and uploaded.
4. The daemon submits an event referencing those uploaded object IDs.
5. The server serializes the event and assigns the next workspace sequence number.
6. The server broadcasts the accepted event to connected clients via WebSocket.
7. Other clients download the referenced encrypted objects, decrypt locally, and apply the update atomically.

## Conflict Handling

Content paths inside a root have opaque `path_id` values. That includes the root entry itself at `relative_path=""`.

The server tracks the latest accepted sequence for content paths in `path_heads`.

`root_add` and `root_remove` are special root-lifecycle events. They do not use `path_id` or `base_seq`. The server accepts them according to root state instead:

- `root_add` is accepted only when the root is absent or currently removed
- `root_remove` is accepted only when the root is currently active

When a client uploads a content change, it includes `base_seq`, meaning "the latest sequence for this path that I built this update from".

- If `base_seq` matches the server's current path head, the event is accepted.
- If it does not match, the server rejects the event with `409 path_head_mismatch`.
- The client keeps both versions by creating a conflict copy locally and re-uploading the losing version under a conflict path.

This preserves data without silent overwrite.

## Bootstrap Model

New or reconnecting clients do not replay the entire history forever.

For bootstrap, the server keeps:

- the latest encrypted snapshot object for each active root
- a workspace-global retained event range after the bootstrap floor

Bootstrap sequence:

1. download latest root snapshots and descriptors
2. decrypt each snapshot locally and materialize or stage all unblocked roots
3. fetch retained events once from the workspace-global bootstrap cursor
4. replay those events in sequence order
5. open live WebSocket subscription

## Local Process Model

The CLI never talks to the server directly. It talks to the local agent over a Unix domain socket.

That gives one place for:

- key storage
- active websocket session
- inotify watches
- retry queues
- local state

The daemon is the only process allowed to:

- keep the authenticated server session token
- own the active WebSocket connection
- mutate sync state in `client.db`
- install or remove filesystem watches
- submit or apply sync events

## Daemon To Server Communication

Only the daemon communicates with the backend.

It uses:

- HTTPS for session start and finish
- HTTPS for bootstrap, event fetch, and event submit
- HTTPS for object upload and object download
- WebSocket for newly accepted events after catch-up completes

The steady-state rule is:

1. establish or renew the HTTP bearer-token session
2. catch up missing history with `/v1/events`, or fall back to `/v1/bootstrap` if required
3. open the WebSocket live feed
4. keep reconnecting with backoff if the socket or session fails

## Restart And Reboot Flow

When the Linux user service starts `syna daemon` after login or reboot, the daemon must:

1. load config, keyring, and local SQLite state
2. bind `~/.local/state/syna/agent.sock`
3. restore active roots, blocked roots, and pending operations from the local DB
4. authenticate to the server and refresh the session if needed
5. catch up remote state with `/v1/events`, or run `/v1/bootstrap` if incremental catch-up is no longer valid
6. reconcile every active root against local disk so changes made while the daemon was not running are turned into pending operations rather than missed
7. install recursive watchers for active roots
8. enter `live` state only after catch-up and reconciliation complete

If remote catch-up and startup reconciliation both produce a change for the same path, the daemon must preserve both versions using the normal conflict-file rule. Restart must never silently discard local bytes.

## Deployment Model

The intended deployment is one VPS and one `syna-server` container with a persistent host volume:

- SQLite database file in the mounted volume
- encrypted objects in the mounted volume
- temporary upload files in the mounted volume

TLS termination is expected to happen in front of the container through Coolify, Caddy, Nginx, or Traefik.
