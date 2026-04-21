# Syna Data Model And Storage

## Canonical Path Rules

All path handling in client code must use one canonical form.

Rules:

1. Expand leading `~` to the current user's home directory.
2. Convert to absolute path.
3. Clean with `filepath.Clean`.
4. Reject the path if it is not under `$HOME`.
5. Reject the path if it overlaps an existing root.
6. Convert the stored root path to slash-separated path relative to `$HOME`.
7. The stored root path must not have a leading `./` or trailing slash.

Examples:

- `/home/alice/.obsidian` -> `.obsidian`
- `/home/alice/projects/wiki` -> `projects/wiki`
- `/etc/nginx` -> reject

## Root Model

A root is the top-level tracked item in a workspace.

Fields:

- `root_id`
- `kind`: `file` or `dir`
- `home_rel_path`
- `status`: `active` or `removed`

All content under a directory root is synchronized recursively.

If a previously removed `home_rel_path` is added again, it reuses the same deterministic `root_id` and starts a fresh root incarnation.

## Event Model

Events are append-only and ordered by a workspace-global sequence number assigned by the server.

Supported event types:

- `root_add`
- `root_remove`
- `dir_put`
- `file_put`
- `delete`

There is no dedicated `rename` event in v1.

Rules:

- `root_add` and `root_remove` are root-lifecycle events
- `dir_put`, `file_put`, and `delete` are content events
- content events use `path_id` and `base_seq`
- root-lifecycle events do not use `path_id` or `base_seq`

## Snapshot Model

A snapshot is the full encrypted logical state of one root at a given accepted sequence.

Each snapshot contains:

- `root_id`
- `base_seq`
- every live path under that root
- enough metadata to recreate the directory tree
- file chunk references for each file entry

Snapshots exist only to speed up bootstrap and recovery. Live sync still happens through incremental events.

The server cannot read encrypted snapshot contents, so the client must also submit the deduplicated set of chunk object IDs reachable from the snapshot for reference tracking.

## Server Storage Layout

Mounted data directory:

```text
/var/lib/syna/
  state.db
  objects/
  tmp/
```

Object file layout:

```text
/var/lib/syna/objects/ab/cd/<object_id>.bin
```

Where:

- `ab` is the first 2 hex chars of the object ID
- `cd` is the next 2 hex chars

This avoids too many files in one directory.

## Server SQLite Schema

This is the logical schema. Exact SQL names may differ, but the stored meaning must stay the same.

### `workspaces`

- `workspace_id TEXT PRIMARY KEY`
- `auth_pubkey BLOB NOT NULL`
- `current_seq INTEGER NOT NULL`
- `retained_floor_seq INTEGER NOT NULL DEFAULT 0`
- `created_at TIMESTAMP NOT NULL`
- `updated_at TIMESTAMP NOT NULL`

### `devices`

- `workspace_id TEXT NOT NULL`
- `device_id TEXT NOT NULL`
- `display_name TEXT NOT NULL`
- `first_seen_at TIMESTAMP NOT NULL`
- `last_seen_at TIMESTAMP NOT NULL`
- primary key: `(workspace_id, device_id)`

### `sessions`

- `token_hash BLOB PRIMARY KEY`
- `workspace_id TEXT NOT NULL`
- `device_id TEXT NOT NULL`
- `issued_at TIMESTAMP NOT NULL`
- `expires_at TIMESTAMP NOT NULL`
- `last_used_at TIMESTAMP NOT NULL`

### `roots`

- `workspace_id TEXT NOT NULL`
- `root_id TEXT NOT NULL`
- `kind TEXT NOT NULL`
- `descriptor_blob BLOB NOT NULL`
- `created_seq INTEGER NOT NULL`
- `removed_seq INTEGER NULL`
- `latest_snapshot_object_id TEXT NULL`
- `latest_snapshot_seq INTEGER NULL`
- `created_at TIMESTAMP NOT NULL`
- `updated_at TIMESTAMP NOT NULL`
- primary key: `(workspace_id, root_id)`

`descriptor_blob` is an encrypted payload containing at least:

- `root_id`
- `kind`
- `home_rel_path`

`created_seq` is the sequence of the current active incarnation. If a removed root is added again, the server resets `created_seq`, clears `removed_seq`, and treats it as a fresh add of the same deterministic `root_id`.

### `path_heads`

- `workspace_id TEXT NOT NULL`
- `root_id TEXT NOT NULL`
- `path_id TEXT NOT NULL`
- `entry_kind TEXT NOT NULL`
- `current_seq INTEGER NOT NULL`
- `deleted INTEGER NOT NULL`
- `updated_at TIMESTAMP NOT NULL`
- primary key: `(workspace_id, root_id, path_id)`

This table exists only so the server can enforce `base_seq` optimistic concurrency on opaque paths.

It stores content-path heads only. `root_add` and `root_remove` do not create `path_heads` rows.

### `events`

- `seq INTEGER PRIMARY KEY AUTOINCREMENT`
- `workspace_id TEXT NOT NULL`
- `root_id TEXT NOT NULL`
- `path_id TEXT NULL`
- `event_type TEXT NOT NULL`
- `base_seq INTEGER NULL`
- `author_device_id TEXT NOT NULL`
- `payload_blob BLOB NOT NULL`
- `created_at TIMESTAMP NOT NULL`

`payload_blob` is the encrypted event payload stored exactly as uploaded by the client.

For `root_add` and `root_remove`, `path_id` and `base_seq` are `NULL`.

### `event_object_refs`

- `event_seq INTEGER NOT NULL`
- `object_id TEXT NOT NULL`
- primary key: `(event_seq, object_id)`

### `snapshots`

- `workspace_id TEXT NOT NULL`
- `root_id TEXT NOT NULL`
- `object_id TEXT NOT NULL`
- `base_seq INTEGER NOT NULL`
- `author_device_id TEXT NOT NULL`
- `created_at TIMESTAMP NOT NULL`
- primary key: `(workspace_id, root_id, object_id)`

### `snapshot_object_refs`

- `snapshot_object_id TEXT NOT NULL`
- `object_id TEXT NOT NULL`
- primary key: `(snapshot_object_id, object_id)`

### `objects`

- `object_id TEXT PRIMARY KEY`
- `kind TEXT NOT NULL`
- `size_bytes INTEGER NOT NULL`
- `storage_rel_path TEXT NOT NULL`
- `ref_count INTEGER NOT NULL`
- `created_at TIMESTAMP NOT NULL`
- `last_accessed_at TIMESTAMP NOT NULL`

`ref_count` counts references from:

- `event_object_refs`
- the snapshot object rows in `snapshots`
- the retained file chunk references in `snapshot_object_refs`

## Client Local Storage

Use XDG paths:

```text
~/.config/syna/
  config.json
  keyring.json

~/.local/state/syna/
  client.db
  agent.sock
  daemon.pid
```

## Client Config

`config.json` stores non-secret local settings:

```json
{
  "device_id": "uuid-v4",
  "device_name": "hostname",
  "server_url": "https://server.example.com",
  "workspace_id": "9fd1e0c1aabbccdd1122334455667788",
  "daemon_auto_start": true
}
```

`daemon_auto_start` means:

- `true`: CLI commands install or refresh `~/.config/systemd/user/syna.service`, require user systemd for automatic startup, and enable the service for future logins after a successful connect
- `false`: Syna does not enable persistent auto-start on its own

`server_url` and `workspace_id` are `null` or absent after `syna disconnect`.
The local keyring's stored recovery key is also removed, so `syna key show`
fails until the client connects to a workspace again.

## Client SQLite Schema

### `workspace_state`

- `singleton INTEGER PRIMARY KEY CHECK (singleton = 1)`
- `server_url TEXT NULL`
- `workspace_id TEXT NULL`
- `session_token TEXT NULL`
- `session_expires_at TIMESTAMP NULL`
- `last_server_seq INTEGER NOT NULL`
- `connection_state TEXT NOT NULL`
- `last_error TEXT NULL`

This table has exactly one row in v1 because each installation handles only one active workspace.

### `roots`

- `root_id TEXT PRIMARY KEY`
- `kind TEXT NOT NULL`
- `home_rel_path TEXT NOT NULL`
- `target_abs_path TEXT NOT NULL`
- `state TEXT NOT NULL`
- `latest_snapshot_seq INTEGER NOT NULL DEFAULT 0`
- `last_scan_at TIMESTAMP NULL`

`state` is one of:

- `active`
- `blocked_nonempty`
- `removed`

### `entries`

- `root_id TEXT NOT NULL`
- `rel_path TEXT NOT NULL`
- `path_id TEXT NOT NULL`
- `kind TEXT NOT NULL`
- `current_seq INTEGER NOT NULL`
- `content_sha256 TEXT NULL`
- `size_bytes INTEGER NULL`
- `mode INTEGER NOT NULL`
- `mtime_ns INTEGER NOT NULL`
- `inode INTEGER NULL`
- `device INTEGER NULL`
- `deleted INTEGER NOT NULL`
- primary key: `(root_id, rel_path)`

### `pending_ops`

- `op_id TEXT PRIMARY KEY`
- `root_id TEXT NOT NULL`
- `rel_path TEXT NOT NULL`
- `op_type TEXT NOT NULL`
- `base_seq INTEGER NOT NULL`
- `payload_json TEXT NOT NULL`
- `status TEXT NOT NULL`
- `retry_count INTEGER NOT NULL`
- `last_error TEXT NULL`
- `created_at TIMESTAMP NOT NULL`

### `ignore_events`

- `root_id TEXT NOT NULL`
- `rel_path TEXT NOT NULL`
- `expires_at TIMESTAMP NOT NULL`
- primary key: `(root_id, rel_path)`

This table suppresses watcher feedback from Syna's own file writes.

## Snapshot Payload Shape

After decryption, a snapshot is canonical JSON with this shape:

```json
{
  "root_id": "hex",
  "kind": "dir",
  "home_rel_path": ".obsidian",
  "base_seq": 120,
  "entries": [
    {
      "path": "",
      "kind": "dir",
      "mode": 493,
      "mtime_ns": 1710000000000000000
    },
    {
      "path": "Daily/2026-04-17.md",
      "kind": "file",
      "mode": 420,
      "mtime_ns": 1710000000000000000,
      "size_bytes": 2451,
      "content_sha256": "hex",
      "chunks": [
        {
          "object_id": "hex",
          "plain_size": 2451
        }
      ]
    }
  ]
}
```

Rules:

- `path=""` is the root itself
- `path=""` is valid for both directory roots and file roots
- directory entries must exist before child entries are applied
- file entries reference already-uploaded encrypted objects

## Retention Rules

- `workspaces.retained_floor_seq` is a workspace-global incremental catch-up floor.
- If any active root has no snapshot yet, `retained_floor_seq` stays `0`.
- If at least one active root exists and all active roots have snapshots, `retained_floor_seq` becomes the minimum `latest_snapshot_seq` across active roots.
- If no active roots remain, `retained_floor_seq` may advance to `current_seq`.
- `GET /v1/events` with `after_seq < retained_floor_seq` must return `410 resync_required`.
- Events with `seq <= retained_floor_seq` may be pruned after retention.
- Pruning an event decrements object refcounts through `event_object_refs`.
- Superseded snapshots and removed-root snapshots may be pruned after retention; pruning them decrements both the snapshot object's refcount and the retained chunk refs in `snapshot_object_refs`.
- Zero-ref objects are eligible for deletion after a configurable retention window.
