# Syna Crypto And Trust Model

## Security Objective

The backend must be able to store and relay synchronized data without being able to reconstruct original file contents or paths from stored data alone.

## Recovery Key

Each workspace is identified by a single shared recovery key.

Raw form:

- 32 random bytes from `crypto/rand`

Display form:

- prefix: `syna1-`
- body: 64 lowercase hex characters of the raw key
- checksum: 8 lowercase hex characters, equal to the first 4 bytes of `SHA256(raw_key)`

Example shape:

```text
syna1-<64-hex>-<8-hex-checksum>
```

The checksum is only for manual entry validation. It is not a cryptographic MAC.

## Key Derivation

Use HKDF-SHA256 with:

- extract salt: literal bytes `syna-v1`
- input key material: raw 32-byte recovery key

Expand the following 32-byte values:

- `k_workspace_id` with info `workspace-id`
- `k_path_id` with info `path-id`
- `k_blob` with info `blob`
- `k_event` with info `event`
- `k_snapshot` with info `snapshot`
- `k_auth_seed` with info `auth-ed25519-seed`

## Workspace ID

Compute:

```text
workspace_id = hex(HMAC-SHA256(k_workspace_id, "workspace"))[0:32]
```

This yields a stable opaque identifier used by the server.

## Authentication

Authentication must not require the server to store a secret derived from the workspace key.

Use:

- Ed25519 private key: `ed25519.NewKeyFromSeed(k_auth_seed)`
- Ed25519 public key: derived from that private key

Server-side storage:

- store only the public key for each workspace

Handshake:

1. client asks to start a session for `workspace_id`
2. server returns a random `server_nonce`
3. client signs the authentication transcript with the derived Ed25519 private key
4. server verifies with the stored public key
5. server returns a short-lived bearer session token

If the server database is stolen, the attacker gains:

- public keys
- opaque identifiers
- encrypted payloads

The attacker does not gain:

- the recovery key
- a reusable signing secret
- any plaintext file content

## Encryption Algorithms

Use `XChaCha20-Poly1305` for all encrypted payloads.

Reasons:

- safe random nonces
- large nonce space
- good Go support
- suitable for client-side encryption of many blobs

## Encrypted Blob Format

Every stored object is a complete opaque binary blob with this layout:

```text
byte 0      : format version = 1
bytes 1..24 : 24-byte XChaCha20 nonce
bytes 25..N : ciphertext with Poly1305 tag appended by the AEAD implementation
```

Object ID:

```text
object_id = hex(SHA256(full_stored_blob))
```

The server verifies that the uploaded body hashes to the provided object ID.

## Additional Authenticated Data

When encrypting file chunks, use AAD:

```text
"syna-blob-v1" || 0x00 || workspace_id || 0x00 || root_id || 0x00 || path_id || 0x00 || chunk_index || 0x00 || plain_size
```

When encrypting event payloads, use AAD:

```text
"syna-event-v1" || 0x00 || workspace_id || 0x00 || root_id || 0x00 || path_id || 0x00 || event_type
```

For `root_add` and `root_remove`, the AAD `path_id` field is the empty string because those events do not carry a content-path identifier in the protocol.

When encrypting snapshot payloads, use AAD:

```text
"syna-snapshot-v1" || 0x00 || workspace_id || 0x00 || root_id || 0x00 || base_seq
```

## Path And Root Identifiers

The server must compare paths for concurrency without learning the actual path.

Use:

```text
root_id = hex(HMAC-SHA256(k_path_id, "root" || 0x00 || home_rel_path))
path_id = hex(HMAC-SHA256(k_path_id, root_id || 0x00 || relative_path))
```

Where:

- `home_rel_path` is the root path relative to `$HOME`
- `relative_path` is the path inside the root using `/`
- `relative_path=""` means the root entry itself

These identifiers are stable and opaque.

## What The Server Can Learn

The server can still observe:

- workspace existence
- number of roots
- event frequency
- approximate file sizes after encryption overhead
- object counts
- client IP addresses and connection times
- device IDs and device names if stored in plain text
- operation types such as `file_put` or `delete`

The server must not be able to observe:

- plaintext file bytes
- plaintext root paths
- plaintext relative paths
- plaintext file modes or mtimes
- plaintext snapshot contents

## Client Key Storage

The product requirement explicitly allows storing the recovery key in plain text on the client.

Store it at:

```text
~/.config/syna/keyring.json
```

Format:

```json
{
  "server_url": "https://server.example.com",
  "workspace_id": "...",
  "workspace_key": "syna1-..."
}
```

When the user runs `syna disconnect`, the client deletes `server_url`, `workspace_id`, and `workspace_key` from this file.

`syna key show` reads this local file and prints the stored `workspace_key` value. It does not retrieve or recover a key from the server.

## Key Rotation

Not supported in v1.

Reason:

- rotating content keys would require re-encrypting all server objects
- rotating auth keys would require a coordinated client and server migration

If key rotation is added later, it must be a v2 feature with a dedicated migration design.

## Recovery Properties

- Lose the recovery key everywhere: the workspace becomes permanently unreadable.
- Lose the server volume: clients still have plaintext local copies and can repopulate the workspace by reconnecting and re-adding roots if necessary.
- Lose a client device: any other device with the recovery key can continue syncing.
