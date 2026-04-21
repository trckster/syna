# Syna Build Plan

## Expected Repo Layout

Recommended Go monorepo layout:

```text
cmd/
  syna/
  syna-server/

internal/
  common/
    config/
    paths/
    protocol/
    crypto/
  client/
    agentrpc/
    connector/
    scanner/
    watcher/
    uploader/
    applier/
    snapshotter/
    state/
  server/
    api/
    db/
    objectstore/
    hub/
    admin/

deploy/
  docker/
    Dockerfile.server
    compose.example.yml
```

The current implementation folds the originally planned `server/auth` package
into `server/api` and `server/db`, and folds the originally planned `server/gc`
package into `server/admin` plus `server/db` pruning helpers.

## Implementation Order

### Phase 1: shared primitives

Build first:

- config loading
- path canonicalization and overlap detection
- workspace key parsing and checksum validation
- HKDF derivation
- object encryption and decryption
- root ID and path ID derivation

### Phase 2: server persistence and auth

Build next:

- SQLite schema and migrations
- object store writer and reader
- session start and finish endpoints
- bearer token validation

### Phase 3: server sync API

Build next:

- bootstrap endpoint
- object upload and download
- event submit
- event fetch
- snapshot submit
- WebSocket hub

### Phase 4: client local state and daemon

Build next:

- local SQLite schema
- config and keyring files
- Unix socket RPC
- auto-start daemon behavior
- `syna connect <server-url>`, `syna disconnect`, `syna key show`, `syna status`, and CLI help

### Phase 5: initial sync

Build next:

- `syna add`
- full root scan
- file chunk upload
- root add and initial event submission
- snapshot creation

### Phase 6: bootstrap and remote apply

Build next:

- new-device bootstrap from snapshot plus events
- empty-target validation
- blocked root state handling
- atomic file materialization

### Phase 7: live watching and conflicts

Build next:

- recursive inotify
- rescan diffing
- pending op queue
- reconnect loop
- conflict copy generation

### Phase 8: packaging and ops

Build last:

- `syna-server` Docker image
- health endpoints
- admin commands
- example compose file
- Coolify notes

## Test Matrix

### Unit tests

- recovery key parse and checksum validation
- path canonicalization
- root overlap rejection
- root ID and path ID stability
- XChaCha20 encryption and decryption
- object ID hashing
- auth signature transcript verification

### Integration tests

- create workspace on empty server
- join existing workspace with recovery key
- reject connecting to a different server until `syna disconnect`
- add directory root with nested files
- bootstrap second client into empty target path
- reject bootstrap into non-empty target path
- edit file on client A and observe client B update
- delete file on client A and observe client B delete
- concurrent edit conflict creates conflict copy
- disconnect client, queue local ops, reconnect and converge
- remove root and verify all clients stop syncing it
- re-add a previously removed root path and verify it behaves like a fresh add
- `syna disconnect` clears local workspace state without deleting local files

### Backend tests

- event commit is atomic
- object upload is idempotent
- path head mismatch returns `409`
- event fetch ordering is stable
- workspace retained floor causes old event fetch to return `410`
- snapshot submit preserves file-chunk refs after old events are compacted
- root removal eventually causes that root's encrypted objects to become zero-ref and GC-able
- GC removes zero-ref objects after retention

### Client tests

- self-change suppression avoids upload loops
- rename is handled as delete plus put
- unsupported special files surface warnings
- temp file write plus atomic rename preserves file integrity

## Definition Of Done

The implementation is done only when:

- all commands from the product scope work
- the server volume contains only encrypted data and opaque metadata
- a second Linux client can restore and stay live-synced
- concurrent edits do not silently destroy data
- `syna disconnect` fully detaches one installation without deleting its local files
- Docker deployment on one VPS is documented and reproducible
