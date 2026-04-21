# Syna

Syna keeps selected folders in sync between your Linux devices through a server
you control. The server stores only encrypted sync metadata and object blobs; the
workspace key stays on the clients, so the server can relay and retain data
without being able to read file contents.

## Installation

### #1 Server Installation

Deploy one Syna server and give it persistent storage. The server should be
reachable over HTTPS at the same public URL your clients will use, for example
`https://syna.example.com`.

In Coolify:

1. Create a new resource and point Coolify at this repository. Use `Public
   Repository` for a public repo, or the appropriate GitHub App / Deploy Key
   option for a private repo.
2. After Coolify checks the repository, set the build pack to `Dockerfile`.
3. Set the Dockerfile location to `/deploy/docker/Dockerfile.server`.
4. In Network, set `Ports Exposes` to `8080` and leave `Port Mappings` empty.
5. In Domains, set the HTTPS URL clients will use, for example
   `https://syna.example.com`.
6. In Persistent Storage, add a Docker volume:
   - Name: `syna-data`
   - Source Path: leave empty
   - Destination Path: `/var/lib/syna`
7. In Healthcheck, set port to `8080` and path to `/readyz`.

Do not use container-local ephemeral storage for `/var/lib/syna`; it contains
the SQLite database and encrypted object store.

For non-Coolify Docker deployments, see [`deploy/README.md`](./deploy/README.md).

### #2 Client Installation

Install the latest client release (Linux only):

```bash
curl -fsSL https://raw.githubusercontent.com/trckster/syna/master/scripts/install.sh | sh
```

## First Use

Connect the first Linux device to your server:

```bash
syna connect https://syna.example.com
```

Start syncing a folder:

```bash
syna add "$HOME/Documents"
```

Connect another Linux device to the same workspace:

```bash
syna connect https://syna.example.com
```

Enter the recovery key from the first device when prompted.

## Client Commands

```text
syna connect <server-url>   Connect this device to a Syna server
syna disconnect             Disconnect this device and stop local sync
syna key show               Print the stored workspace recovery key
syna add <path>             Start syncing a file or directory
syna rm <path>              Stop syncing a path without deleting local files
syna status                 Show sync and connection state
syna uninstall              Remove Syna from this client
syna version                Show client version
syna help                   Show command help
```

`syna connect` starts or contacts the local background daemon automatically when
`daemon_auto_start` is enabled. Client config is stored under
`$XDG_CONFIG_HOME/syna`, and local state is stored under `$XDG_STATE_HOME/syna`.
After `syna disconnect`, the local stored recovery key is removed; reconnecting
that device requires entering a recovery key again.

On systems with user systemd, the daemon is managed as:

```bash
systemctl --user status syna.service
```

## Uninstall

To remove Syna from a Linux client:

```bash
syna uninstall
```

## Server Operations

Production servers should run behind HTTPS with WebSocket upgrade support for
`/v1/ws`. The raw backend listener should not be exposed directly to the public
internet.

Useful server commands:

```text
syna-server serve
syna-server migrate
syna-server gc
syna-server stats
syna-server doctor
syna-server version
```

See [`deploy/README.md`](./deploy/README.md) for Docker Compose, Coolify,
reverse-proxy, backup, restore, and upgrade guidance.

## Development

Local development requires Linux, Go `1.25.9`, CGO-enabled SQLite build support,
and a C compiler such as `build-essential` on Debian or Ubuntu. For Linux ARM64
release archives, install `gcc-aarch64-linux-gnu` as well.

Build both binaries into `./bin`:

```bash
mkdir -p ./bin
go build -o ./bin/syna ./cmd/syna
go build -o ./bin/syna-server ./cmd/syna-server
```

Run tests:

```bash
go test ./...
go vet ./...
./scripts/coverage.sh
./scripts/smoke.sh
```

Build release archives:

```bash
./scripts/release.sh
```

## Known Limitations

- Linux only.
- One workspace per client installation.
- Backend horizontal scaling is unsupported for v1.
- Symlinks and special files are skipped and surfaced as warnings.
- Renames sync as delete plus put.
- Local HTTP requires `SYNA_ALLOW_HTTP=true` and is only for development.
