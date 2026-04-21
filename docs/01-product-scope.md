# Syna Product Scope

## Goal

Build a Linux-only file sync application that:

- syncs chosen directories and files across multiple Linux devices
- uses a VPS-hosted backend deployed in Docker
- keeps all synchronized file data encrypted at rest on the server
- propagates changes from any connected client to all other connected clients almost immediately
- can bootstrap a new client from the same shared secret key

## User-facing Commands

The user-facing CLI for v1 is:

```text
syna connect <server-url>
syna disconnect
syna key show
syna add <path>
syna rm <path>
syna status
syna help
syna -h
syna --help
```

The same binary also contains an internal daemon mode:

```text
syna daemon
```

## Hard Requirements From Product Input

- Backend runs inside Docker.
- Backend is administered from terminal only.
- Backend stores persistent state on mounted volumes, never in the container filesystem layer.
- Client has a daemon that watches tracked paths and syncs changes.
- Client daemon is managed as a per-user Linux service and must resume automatically after reboot for that user.
- New clients with the same secret key can join and restore the same tracked paths.
- If a target path does not exist on a new client, create it.
- If a target directory already exists and is not empty, block sync for that root and show an error.
- If a target file already exists on a new client, block sync for that root and show an error.
- Linux only. No Windows or macOS support.
- Sync is many-way, not primary/secondary.
- Deployment must work with plain Docker and with Coolify.

## Mandatory v1 Interpretations

These are deliberate product decisions added to make the system consistent and safe:

- Syna v1 supports one active workspace per client installation.
- If an installation is already connected to one server, `syna connect <different-server-url>` must fail until the user runs `syna disconnect`.
- Syna v1 ships two binaries, not three: `syna` and `syna-server`. The local daemon is the `syna daemon` mode of `syna`.
- Tracked roots must live under the current user's `$HOME`.
- Stored root paths are home-relative, not absolute. Example: `~/.obsidian` is stored as `.obsidian`.
- On another device, the same root is materialized under that device's `$HOME`.
- Overlapping roots are rejected. If `.obsidian` is tracked, `.obsidian/plugins` cannot be added separately.
- `syna disconnect` stops syncing on the current device only, clears the local workspace association and recovery key, disables the local background service, and does not modify the server workspace or other clients.
- `syna rm <path>` removes that root from the workspace globally, stops syncing it on every client, makes its encrypted server-side history eligible for deletion by retention and GC, and does not delete local files from disk.
- Re-adding a previously removed root path is allowed and must behave like a fresh first-time add.
- Symlinks, sockets, fifos, block devices, char devices, ACLs, xattrs, uid/gid ownership preservation, and file locking are out of scope for v1.
- Rename detection is not special-cased. A rename is represented as delete-old-path plus put-new-path.
- Horizontal backend scaling is out of scope for v1. One backend instance is the intended deployment model.

## Why Roots Are Limited To `$HOME`

This is the only design change that should be treated as mandatory rather than optional. Without it, "restore to the same directories" becomes ambiguous across devices because absolute paths often differ by username, mount layout, or host-specific directories.

By forcing roots to be home-relative:

- the same workspace can restore cleanly on multiple Linux machines
- a new device knows exactly where to create the path locally
- the system avoids syncing host-specific paths such as `/var`, `/etc`, or removable mount points

## Supported File Types

- regular files
- directories

Unsupported file types are ignored and surfaced in status output as warnings.

## Conflict Policy

Silent overwrite is not allowed. If two clients edit the same path concurrently from different base revisions:

- the first accepted update keeps the original path
- the later update is preserved as a conflict copy with a deterministic conflict suffix

## Acceptance Criteria

Syna v1 is complete when all of the following are true:

- A first client can connect to a fresh server, create a workspace, and print a recovery key.
- A connected client can print its locally stored recovery key with `syna key show`.
- A second client can connect to the same server, enter that key once, and restore the same roots under its own `$HOME`.
- `syna help`, `syna -h`, and `syna --help` print the CLI usage summary.
- Adding a root uploads its initial state and begins live watching.
- Editing a file on one client causes the changed content to appear on other connected clients quickly without manual pull commands.
- Removing a root stops syncing it on all clients without deleting local files, and its encrypted server-side history is eventually deleted by retention and GC.
- `syna disconnect` stops background sync for that device and prevents automatic reconnect until the user explicitly connects again.
- The server volume contains only encrypted file data and encrypted path metadata.
- A server operator with full access to the VPS volume cannot reconstruct original file contents from stored encrypted blobs alone.
