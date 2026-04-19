# Syna Sync Protocol

## Transport

- HTTPS for request/response APIs
- WebSocket for live accepted-event fanout
- JSON for structured API bodies
- `application/octet-stream` for object upload and download

The backend may reject plain HTTP in production. A development escape hatch may exist, but production builds must assume HTTPS.

## Common Headers

- `Authorization: Bearer <session-token>` for authenticated endpoints
- `X-Syna-Protocol: 1`

## Session Start

### `POST /v1/session/start`

Purpose:

- begin authentication
- create a workspace if it does not exist and `create_if_missing=true`

Request body:

```json
{
  "workspace_id": "9fd1e0c1aabbccdd1122334455667788",
  "device_id": "8d1c7d8d-1f2e-4b3b-a9aa-6f5e3e9cc111",
  "device_name": "laptop-rome",
  "client_nonce": "base64",
  "create_if_missing": true,
  "workspace_pubkey": "base64"
}
```

Rules:

- `workspace_pubkey` is required only when creating a new workspace
- if the workspace already exists, `workspace_pubkey` must either be omitted or match the stored public key

Successful response:

```json
{
  "workspace_exists": true,
  "created": false,
  "server_nonce": "base64",
  "server_time": "2026-04-17T12:34:56Z",
  "protocol_version": 1
}
```

Server behavior:

- create or reuse a short-lived challenge record
- challenge lifetime: 60 seconds

## Session Finish

### `POST /v1/session/finish`

The client signs:

```text
SHA256("syna-auth-v1" || 0x00 || workspace_id || 0x00 || device_id || 0x00 || client_nonce || 0x00 || server_nonce)
```

Request body:

```json
{
  "workspace_id": "9fd1e0c1aabbccdd1122334455667788",
  "device_id": "8d1c7d8d-1f2e-4b3b-a9aa-6f5e3e9cc111",
  "client_nonce": "base64",
  "server_nonce": "base64",
  "signature": "base64"
}
```

Successful response:

```json
{
  "session_token": "base64url-random",
  "expires_at": "2026-04-18T12:34:56Z",
  "workspace_id": "9fd1e0c1aabbccdd1122334455667788",
  "current_seq": 120
}
```

Session token rules:

- random 32 bytes, base64url encoded
- stored server-side only as `SHA256(token)`
- default TTL: 24 hours
- renewed automatically by the client before expiry

## Bootstrap

### `GET /v1/bootstrap`

Response:

```json
{
  "workspace_id": "9fd1e0c1aabbccdd1122334455667788",
  "current_seq": 120,
  "bootstrap_after_seq": 100,
  "roots": [
    {
      "root_id": "hex",
      "kind": "dir",
      "descriptor_blob": "base64",
      "created_seq": 3,
      "removed_seq": null,
      "latest_snapshot_object_id": "hex",
      "latest_snapshot_seq": 100
    }
  ]
}
```

`descriptor_blob` is an encrypted payload that contains at least:

- `root_id`
- `kind`
- `home_rel_path`

Rules:

- only active roots are returned
- `bootstrap_after_seq` is the single workspace-global cursor the client must use after materializing snapshots or empty roots
- if a root has a snapshot, its bootstrap contribution is `latest_snapshot_seq`
- if a root has no snapshot yet, its bootstrap contribution is `created_seq - 1`
- `bootstrap_after_seq` is the minimum bootstrap contribution across all active roots
- if no active roots exist, `bootstrap_after_seq` equals `current_seq`

## Object Upload

### `PUT /v1/objects/{object_id}`

Headers:

- `Content-Type: application/octet-stream`
- `X-Syna-Object-Kind: file_chunk` or `snapshot`
- `X-Syna-Plain-Size: <integer>`

Body:

- the fully encrypted binary object blob

Behavior:

- idempotent
- if the object already exists, return `200`
- if it is new, store it and return `201`
- if the request body hash does not match `{object_id}`, return `400`

## Object Download

### `GET /v1/objects/{object_id}`

Response body:

- the exact encrypted object bytes as originally uploaded

## Event Submit

### `POST /v1/events`

Request body:

```json
{
  "root_id": "hex",
  "path_id": "hex",
  "event_type": "file_put",
  "base_seq": 119,
  "payload_blob": "base64",
  "object_refs": [
    "hex"
  ]
}
```

Rules:

- `payload_blob` is the full encrypted event payload
- `object_refs` must already exist in object storage
- `root_add` and `root_remove` omit `path_id` and `base_seq`
- `root_add` and `root_remove` send `object_refs: []`
- `dir_put`, `file_put`, and `delete` require `path_id` and `base_seq`
- for content events, `base_seq` must match the current `path_heads.current_seq` for that path, except for never-before-seen paths where `base_seq` must be `0`
- `root_add` is accepted only when that `root_id` is absent or currently removed
- if `root_add` reactivates a previously removed root, the server must clear old `path_heads` and snapshot pointers for that root so the new add behaves like a fresh first-time root
- `root_remove` is accepted only when that `root_id` is currently active
- content events are rejected for removed or unknown roots

Client-side integrity invariants:

- treat decrypted metadata as untrusted until it is locally re-derived
- for `root_add`, recompute `root_id` from decrypted `home_rel_path` and reject any mismatch with the outer record
- for `dir_put`, `file_put`, and `delete`, recompute `path_id` from decrypted `payload.Path` and reject any mismatch with the outer record
- reject absolute, traversal, non-canonical, or out-of-root paths before any filesystem action
- reject `home_rel_path` values that resolve outside `$HOME`

Successful response:

```json
{
  "accepted_seq": 120,
  "workspace_seq": 120
}
```

Conflict response:

```json
{
  "code": "path_head_mismatch",
  "current_seq": 120
}
```

## Event Fetch

### `GET /v1/events?after_seq=<n>&limit=<m>`

Normal response:

```json
{
  "events": [
    {
      "seq": 120,
      "root_id": "hex",
      "path_id": "hex",
      "event_type": "file_put",
      "base_seq": 119,
      "author_device_id": "uuid",
      "payload_blob": "base64",
      "object_refs": [
        "hex"
      ],
      "created_at": "2026-04-17T12:35:30Z"
    }
  ],
  "current_seq": 120
}
```

Rules:

- events are returned in ascending `seq` order
- default `limit`: 100
- maximum `limit`: 1000
- `root_add` and `root_remove` appear with `path_id=null` and `base_seq=null`
- the server does not omit retained events with `seq > retained_floor_seq`

If `after_seq` is older than the workspace retained floor, return:

- status `410 Gone`
- body `{"code":"resync_required","retained_floor_seq":100}`

## Snapshot Submit

### `POST /v1/snapshots`

Request body:

```json
{
  "root_id": "hex",
  "base_seq": 120,
  "object_id": "hex",
  "object_refs": [
    "hex"
  ]
}
```

Rules:

- `object_id` must already exist in the object store
- the uploaded object must be an encrypted snapshot object
- `base_seq` is the sequence the snapshot represents
- `object_refs` is the deduplicated set of file-chunk object IDs reachable from the decrypted snapshot payload
- the server increments refcounts for the snapshot object itself and for every referenced chunk object

Successful response:

```json
{
  "root_id": "hex",
  "base_seq": 120,
  "object_id": "hex"
}
```

## WebSocket Live Feed

### `GET /v1/ws`

Authentication:

- same bearer token as HTTP endpoints

Behavior:

- after auth, the socket receives only newly accepted events
- backlog catch-up is done through `GET /v1/events`, not through the WebSocket

Server messages:

```json
{
  "type": "event",
  "event": {
    "seq": 121,
    "root_id": "hex",
    "path_id": "hex",
    "event_type": "delete",
    "base_seq": 120,
    "author_device_id": "uuid",
    "payload_blob": "base64",
    "object_refs": [],
    "created_at": "2026-04-17T12:36:01Z"
  }
}
```

Heartbeat:

```json
{
  "type": "ping",
  "server_time": "2026-04-17T12:36:30Z"
}
```

Client reply:

```json
{
  "type": "pong"
}
```

## Decrypted Event Payload Shapes

These structures are client-side only. The server stores them as opaque encrypted bytes.

### `root_add`

```json
{
  "root_id": "hex",
  "kind": "dir",
  "home_rel_path": ".obsidian"
}
```

### `root_remove`

```json
{
  "root_id": "hex"
}
```

### `dir_put`

```json
{
  "path": "Daily",
  "mode": 493,
  "mtime_ns": 1710000000000000000
}
```

`path=""` means the root entry itself.

### `file_put`

```json
{
  "path": "Daily/2026-04-17.md",
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
```

`path=""` means the root file itself.

### `delete`

```json
{
  "path": "Daily/old.md"
}
```

## Snapshot Cadence

The client should upload a new snapshot for a root when either condition is true:

- 100 accepted events happened for that root since the last snapshot
- 5 minutes passed since the last snapshot and there was at least one accepted event

This keeps bootstrap bounded without forcing snapshot upload after every small change.
