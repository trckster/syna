# Deployment

This repository ships a server container build in [`deploy/docker/Dockerfile.server`](./docker/Dockerfile.server) and a local reference stack in [`deploy/docker/compose.example.yml`](./docker/compose.example.yml).

## Docker Compose

1. Copy `deploy/docker/compose.example.yml` to your deployment host.
2. Set `SYNA_PUBLIC_BASE_URL` to the public HTTPS origin clients will use.
3. Mount a persistent host directory to `/var/lib/syna`.
4. Keep the published backend port bound to localhost and place an HTTPS reverse proxy in front.

Persistent data under `/var/lib/syna`:

- `state.db`
- `objects/`
- `tmp/`

## Coolify

Recommended Coolify settings:

- Resource type: Public Repository
- Build pack: Dockerfile
- Base directory: `/`
- Dockerfile location: `/deploy/docker/Dockerfile.server`
- Domains: the HTTPS URL you set in `SYNA_PUBLIC_BASE_URL`
- Ports Exposes: `8080`
- Port Mappings: leave empty
- Persistent Storage: add a Docker volume with name `syna-data`, empty source
  path, and destination path `/var/lib/syna`
- Healthcheck: enabled, path `/readyz`, port `8080` if Coolify shows a port
  field

No Coolify command override is required; the Dockerfile already runs
`syna-server serve`. Leave custom Docker options empty. No separate WebSocket
toggle is required when using Coolify's normal domain/proxy path.

Required environment variables:

- `SYNA_PUBLIC_BASE_URL=https://syna.example.com`

The image and application already default to:

- `SYNA_LISTEN=:8080`
- `SYNA_DATA_DIR=/var/lib/syna`
- `SYNA_SESSION_TTL=24h`
- `SYNA_EVENT_RETENTION=24h`
- `SYNA_ZERO_REF_RETENTION=168h`
- `SYNA_READ_HEADER_TIMEOUT=10s`
- `SYNA_READ_TIMEOUT=30s`
- `SYNA_WRITE_TIMEOUT=30s`
- `SYNA_IDLE_TIMEOUT=120s`

Do not deploy Syna with container-local ephemeral storage for `/var/lib/syna`.
If you use a bind mount instead of a Docker volume, set Source Path to a host
directory such as `/srv/syna`, set Destination Path to `/var/lib/syna`, and
ensure the host directory is writable by container UID `10001`.

## Reverse Proxy

Your reverse proxy must provide:

- HTTPS termination
- WebSocket upgrade support for `/v1/ws`
- request body limits large enough for encrypted object uploads and snapshot submissions
  File chunks are limited to 4 MiB plaintext and snapshot objects to 16 MiB plaintext before encryption overhead.
- long-lived idle connections

Do not expose the raw backend listener directly to the public internet. Bind it to localhost or a private interface and publish only the HTTPS reverse-proxy entrypoint.

## Backup And Restore

Recommended backup flow:

1. Stop `syna-server`, or take a SQLite-safe snapshot while it is quiescent.
2. Archive the full persistent volume mounted at `/var/lib/syna`.
3. Store the archive off-host.

Restore flow:

1. Restore the archived directory back to `/var/lib/syna`.
2. Start the same or a newer compatible `syna-server` build.
3. Run `syna-server doctor` and confirm `/readyz` returns `200`.

Because payloads are end-to-end encrypted, server backups are still encrypted at rest, but they remain operationally sensitive and should still be protected.

## Resource Limits And Monitoring

Set explicit limits for:

- free disk space for `/var/lib/syna`
- file descriptors for the reverse proxy and `syna-server`
- container or host memory for SQLite page cache plus object uploads

Monitor:

- disk pressure and object-store growth under `/var/lib/syna/objects`
- HTTP `429` and rejected WebSocket subscriber logs
- object upload failures and request timeout logs

## Upgrade

Recommended upgrade flow:

1. Back up `/var/lib/syna`.
2. Build or pull the new server image.
3. Stop the old container.
4. Start the new container against the same mounted volume.
5. Verify `/readyz`.
6. Run `syna-server doctor`.
7. Optionally run `syna-server gc` after the upgrade window.

Rolling or active-active upgrades are not supported in v1.

## Unsupported Topologies

The following deployments are unsupported:

- two `syna-server` instances pointed at the same `/var/lib/syna` volume
- SQLite on ephemeral container storage
- multiple writers sharing the same `state.db`
- load-balanced multi-instance backends sharing one workspace store

Syna v1 assumes a single server process owns one SQLite database and one object store directory tree.
