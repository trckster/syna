# Syna

Syna keeps selected folders in sync between your Linux devices through a server
you control. The server stores only encrypted sync metadata and object blobs; the
workspace key stays on the clients, so the server can relay and retain data
without being able to read file contents.

> Syna is an early v1 implementation for Linux clients and single-server
> deployments.

## Installation

### Server Installation With Coolify

Deploy one Syna server and give it persistent storage. The server should be
reachable over HTTPS at the same public URL your clients will use, for example
`https://syna.example.com`.

In Coolify:

1. Create a new resource and choose `Public Repository` for this project
   repository. For a private repository, use the appropriate GitHub App or
   Deploy Key option instead.
2. After Coolify checks the repository, set the build pack to `Dockerfile`.
3. Set the base directory to `/`.
4. Set the Dockerfile location to `/deploy/docker/Dockerfile.server`.
5. In Network, set `Ports Exposes` to `8080` and leave `Port Mappings` empty.
6. In Domains, set the HTTPS URL clients will use, for example
   `https://syna.example.com`.
7. In Persistent Storage, add a Docker volume:
   - Name: `syna-data`
   - Source Path: leave empty
   - Destination Path: `/var/lib/syna`
8. In Healthcheck, set port to `8080` and path to `/readyz`.

The Dockerfile already starts the server with `syna-server serve`; no Coolify
start command override or custom Docker option is required. Coolify's normal
domain/proxy path is sufficient for Syna's WebSocket endpoint; no separate
WebSocket toggle is required.

Syna has defaults for its server runtime settings, including listen address,
data directory, session TTLs, retention windows, and HTTP timeouts. For a
production deployment, set only the public URL clients should use:

```text
SYNA_PUBLIC_BASE_URL=https://syna.example.com
```

This value must match the HTTPS URL configured in Coolify's `Domains` field.
Do not use container-local ephemeral storage for `/var/lib/syna`; it contains
the SQLite database and encrypted object store.

### Client Installation On Linux

Install the client from a published package or release binary. You do not need
to download this repository or build from source on client machines.

If a Debian package is available for your release, install it directly:

```bash
sudo apt install ./syna_<version>_linux_amd64.deb
syna version
```

If only release archives are available, install the `syna` binary from the
archive that matches your CPU architecture:

```bash
curl -LO https://example.com/releases/syna-vX.Y.Z-linux-amd64.tar.gz
tar -xzf syna-vX.Y.Z-linux-amd64.tar.gz
sudo install -m 0755 syna-vX.Y.Z-linux-amd64/syna /usr/local/bin/syna
syna version
```

Use `linux-arm64` instead of `linux-amd64` on ARM64 machines.

## First Use

Connect the first Linux device to your server:

```bash
syna connect https://syna.example.com
```

Press Enter at the recovery-key prompt to create a new workspace. Syna prints a
`syna1-...` recovery key once; store it somewhere safe because it is required to
connect other devices.

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
syna add <path>             Start syncing a file or directory
syna rm <path>              Stop syncing a path without deleting local files
syna status                 Show sync and connection state
syna version                Show client version
syna help                   Show command help
```

`syna connect` starts or contacts the local background daemon automatically when
`daemon_auto_start` is enabled. Client config is stored under
`$XDG_CONFIG_HOME/syna`, and local state is stored under `$XDG_STATE_HOME/syna`.

On systems with user systemd, the daemon is managed as:

```bash
systemctl --user status syna.service
```

If user systemd is unavailable, the CLI falls back to launching `syna daemon`
and writes logs to `$XDG_STATE_HOME/syna/daemon.log`.

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

## Release Artifacts

The release build emits Linux archives in `dist/`:

```text
syna-<version>-linux-amd64.tar.gz
syna-<version>-linux-arm64.tar.gz
```

Each archive contains:

```text
syna
syna-server
README.md
```

Client machines only need the `syna` binary. Servers deployed through Coolify
use the Dockerfile at `deploy/docker/Dockerfile.server`.

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

## Documentation

- [`docs/00-index.md`](./docs/00-index.md): documentation index
- [`docs/02-architecture.md`](./docs/02-architecture.md): architecture overview
- [`docs/03-crypto-and-trust.md`](./docs/03-crypto-and-trust.md): crypto and trust model
- [`docs/08-deployment-and-operations.md`](./docs/08-deployment-and-operations.md): operations reference
- [`docs/10-release-checklist.md`](./docs/10-release-checklist.md): release checklist
