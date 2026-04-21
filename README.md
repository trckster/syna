# Syna

Syna keeps selected folders in sync between your Linux devices through a server
you control. The server stores only encrypted sync metadata and object blobs; the
workspace key stays on the clients, so the server can relay and retain data
without being able to read file contents.

> Syna is an early v1 implementation for Linux clients and single-server
> deployments.

## Deployment Model

Syna is distributed to clients through GitHub Releases. Client machines should
download the latest release archive for their CPU architecture and install the
`syna` binary from it; they should not clone this repository or build anything.

The server is deployed from this repository. Coolify can build the server
container directly from the repo, or operators can build the Docker image from
`deploy/docker/Dockerfile.server`. Syna does not currently publish Debian
packages or a public server container image.

## Installation

### Server Installation With Coolify

Deploy one Syna server and give it persistent storage. The server should be
reachable over HTTPS at the same public URL your clients will use, for example
`https://syna.example.com`.

In Coolify:

1. Create a new resource and point Coolify at this repository. Use `Public
   Repository` for a public repo, or the appropriate GitHub App / Deploy Key
   option for a private repo.
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

For non-Coolify Docker deployments, see [`deploy/README.md`](./deploy/README.md).

### Client Installation On Linux

Install the latest client release:

```bash
curl -fsSL https://raw.githubusercontent.com/trckster/syna/main/scripts/install.sh | sh
```

The installer detects `linux-amd64` versus `linux-arm64`, resolves the latest
GitHub Release, downloads the matching archive, verifies the checksum when the
release publishes one, and installs only the client binary to
`/usr/local/bin/syna`. If the per-user `syna.service` is already running, the
installer restarts it after replacing the binary.

The release archive also contains `syna-server`; client machines do not need
that binary. Do not download GitHub's source-code archives for client
installation; they do not contain built binaries. This repository does not build
a `.deb` package.

To install a specific version or use a different destination:

```bash
curl -fsSL https://raw.githubusercontent.com/trckster/syna/main/scripts/install.sh \
  | SYNA_VERSION=v1.2.3 INSTALL_DIR="$HOME/.local/bin" sh
```

`SYNA_VERSION` must match the GitHub Release tag.

For manual upgrades, install the newer `syna` binary over the old one. If the
daemon is already running under user systemd, restart it after replacing the
binary. The one-line installer handles this restart automatically.

```bash
systemctl --user restart syna.service
```

If user systemd is unavailable, stop any manually launched `syna daemon` process
and let the next CLI command start it again.

## First Use

Connect the first Linux device to your server:

```bash
syna connect https://syna.example.com
```

At the recovery-key prompt, press Enter to create a new workspace on a fresh
server. Syna prints a `syna1-...` recovery key. This key lets other devices
join the same encrypted workspace, and anyone who has it can access that
workspace, so store it somewhere safe. On a connected device, show the locally
stored key again with:

```bash
syna key show
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

Maintainers build Linux release archives with `./scripts/release.sh` and upload
them to GitHub Releases:

```text
syna-<version>-linux-amd64.tar.gz
syna-<version>-linux-arm64.tar.gz
syna-<version>-checksums.txt
```

Each archive contains:

```text
syna
syna-server
README.md
```

Client machines only need the `syna` binary from the published release archive.
Servers deployed through Coolify use the Dockerfile at
`deploy/docker/Dockerfile.server`. The release script does not produce Debian
packages.

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
