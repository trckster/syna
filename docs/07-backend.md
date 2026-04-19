# Syna Backend Design

## Deployment Shape

The backend is one process in one Docker container:

- binary: `syna-server`
- persistent state: mounted host volume
- no web UI
- terminal-only administration

## Internal Modules

- `config`: env and flags
- `db`: SQLite transactions and schema migrations
- `objectstore`: object write, read, hash verification, refcount maintenance
- `api`: HTTP and WebSocket handlers, session challenges, signature verification, token issuance
- `hub`: live workspace fanout
- `admin`: terminal subcommands, including GC orchestration

GC bookkeeping and pruning invariants live in `db`; `admin` wires them to the
`syna-server gc` command.

## Why SQLite

SQLite is the right v1 choice because:

- one backend instance is the target deployment
- it keeps Docker deployment simple
- it works well with mounted volumes
- it removes the need for a second service such as Postgres
- WAL mode is enough for the expected write pattern

Required SQLite settings:

- WAL journal mode
- busy timeout enabled
- foreign keys enabled
- synchronous mode `FULL`

## Event Acceptance Transactions

Every accepted event must be committed in one database transaction.

### Content events

These are `dir_put`, `file_put`, and `delete`.

Transaction steps:

1. validate bearer session
2. verify referenced objects exist
3. begin transaction
4. lock or read the workspace row
5. verify the root is active
6. load `path_heads` for `(workspace_id, root_id, path_id)`
7. compare `base_seq`
8. if mismatch:
   - rollback
   - return `409 path_head_mismatch`
9. insert the event row
10. assign the next `seq`
11. upsert the `path_heads` row with the new head sequence
12. increment refcounts for all `object_refs`
13. update `devices.last_seen_at`
14. update `workspaces.current_seq`
15. commit
16. publish the committed event to the WebSocket hub

### Root lifecycle events

These are `root_add` and `root_remove`.

`root_add` and `root_remove` do not use `path_id` or `base_seq`.

`root_add` transaction:

1. validate bearer session
2. begin transaction
3. lock or read the workspace row
4. ensure the root is absent or currently removed
5. insert the `root_add` event row with `path_id=NULL` and `base_seq=NULL`
6. assign the next `seq`
7. upsert the `roots` row with the new descriptor, clear `removed_seq`, clear snapshot pointers, and set `created_seq` to the accepted sequence
8. delete any stale `path_heads` rows for that root so later content events start from `base_seq=0`
9. update `devices.last_seen_at`
10. update `workspaces.current_seq`
11. commit
12. publish the committed event to the WebSocket hub

`root_remove` transaction:

1. validate bearer session
2. begin transaction
3. lock or read the workspace row
4. ensure the root is currently active
5. insert the `root_remove` event row with `path_id=NULL` and `base_seq=NULL`
6. assign the next `seq`
7. set `roots.removed_seq` to the accepted sequence and clear snapshot pointers
8. delete all `path_heads` rows for that root so a future re-add behaves like a fresh first-time root
9. update `devices.last_seen_at`
10. update `workspaces.current_seq`
11. commit
12. publish the committed event to the WebSocket hub

No event may be broadcast before its transaction commits.

## Root Add And Remove Semantics

### Root Add

- the server stores the encrypted root descriptor in `roots`
- the server records the `root_add` event
- the server sets the root as active

### Root Remove

- the server records the `root_remove` event
- the server marks `removed_seq`
- the server clears live `path_heads` and snapshot pointers for that root
- the server does not delete objects immediately, but the removed root's encrypted history becomes eligible for deletion through normal compaction and GC

Root removal is logical first, physical later.

## Object Write Rules

When handling `PUT /v1/objects/{object_id}`:

1. stream the request to a temp file under `/var/lib/syna/tmp`
2. hash bytes while streaming
3. verify hash equals `object_id`
4. `fsync` the temp file
5. atomically rename to the final object path
6. insert or reuse the `objects` row

If two clients upload the same object concurrently:

- only one file should win the final rename
- both requests should still succeed idempotently

## WebSocket Hub

Maintain an in-memory subscriber list per workspace.

When an event commits:

- push it to every connected client in that workspace
- push to the author device too, because the author still needs the canonical accepted sequence

If a client falls behind or the socket drops:

- it reconnects
- it catches up with `GET /v1/events`

## Snapshot Handling

The server stores snapshots as ordinary encrypted objects plus metadata rows.

Rules:

- only the latest snapshot pointer per root matters for bootstrap
- older snapshot objects remain referenced until compaction
- a snapshot does not replace the event log immediately
- the server must also store the deduplicated chunk references submitted with the snapshot so file objects remain referenced after old events are compacted

## Compaction And GC

Three cleanup passes are required.

### 1. Event compaction

Maintain a workspace-global `retained_floor_seq`.

Rules:

- if any active root has no snapshot, `retained_floor_seq = 0`
- if active roots exist and all of them have snapshots, `retained_floor_seq` is the minimum `latest_snapshot_seq` across active roots
- if no active roots remain, `retained_floor_seq` may advance to `workspaces.current_seq`
- retain events with `seq <= retained_floor_seq` for the grace period, then delete them
- never delete retained events with `seq > retained_floor_seq`
- deleting an event must decrement refcounts through `event_object_refs`

Default grace period:

- 24 hours

If a client asks for events older than the retained floor:

- respond `410 resync_required`

### 2. Snapshot compaction

- keep the latest snapshot row per active root
- removed roots do not need to stay bootstrap-ready, so their snapshot rows may be pruned after retention
- deleting a snapshot row must decrement the snapshot object's refcount
- deleting a snapshot row must also decrement every retained chunk ref in `snapshot_object_refs`

### 3. Object garbage collection

Delete objects whose `ref_count` is zero and whose zero-ref age exceeds the retention window.

Default zero-ref retention:

- 7 days

## Admin Commands

The backend image must support terminal subcommands:

```text
syna-server serve
syna-server migrate
syna-server gc
syna-server stats
syna-server doctor
```

Expected behavior:

- `serve`: run the API service
- `migrate`: apply schema migrations and exit
- `gc`: run one compaction and GC pass, then exit
- `stats`: print workspace, object, and sequence counts
- `doctor`: verify DB accessibility, object paths, and schema version

## Health Endpoints

The backend should expose:

- `GET /healthz`: process is alive
- `GET /readyz`: DB and data directory are writable and usable

These are for reverse proxies and Coolify, not for user administration.

## Limits

Recommended defaults:

- max event fetch page: 1000
- max single object plain size: 4 MiB for file chunks
- max request body for event submit: 1 MiB
- max request body for snapshot submit: configurable, default 16 MiB
- max concurrent websocket clients per workspace: configurable, default 32

## Single-instance Limitation

Syna v1 must explicitly reject multi-instance deployment guidance.

Reason:

- SQLite is local
- WebSocket fanout is in-memory
- object storage is local filesystem based

If high availability is needed later, that is a separate architecture.
