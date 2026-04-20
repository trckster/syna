# Syna Deployment And Operations

## Deployment Target

One VPS running:

- one `syna-server` Docker container
- one mounted host directory for persistent state
- one HTTPS reverse proxy in front, or Coolify-managed HTTPS

## Required Reverse Proxy Features

- HTTPS termination
- WebSocket upgrade support
- request body sizes large enough for object uploads and snapshot-submit JSON bodies
- long-lived idle connections for WebSocket

## Container Filesystem Rules

The container must treat `/var/lib/syna` as the only persistent write location.

Everything else in the container can be ephemeral.

Mounted data:

```text
/var/lib/syna/state.db
/var/lib/syna/objects/
/var/lib/syna/tmp/
```

## Required Environment Variables

| Variable | Meaning |
| --- | --- |
| `SYNA_LISTEN` | API listen address, default `:8080` |
| `SYNA_DATA_DIR` | persistent state directory, default `/var/lib/syna` |
| `SYNA_PUBLIC_BASE_URL` | external HTTPS URL clients use |
| `SYNA_SESSION_TTL` | bearer token TTL, default `24h` |
| `SYNA_EVENT_RETENTION` | event grace retention after snapshot compaction, default `24h` |
| `SYNA_ZERO_REF_RETENTION` | zero-ref object retention, default `168h` |
| `SYNA_READ_HEADER_TIMEOUT` | HTTP header read timeout, default `10s` |
| `SYNA_READ_TIMEOUT` | full request read timeout, default `30s` |
| `SYNA_WRITE_TIMEOUT` | response write timeout, default `30s` |
| `SYNA_IDLE_TIMEOUT` | idle keepalive timeout, default `120s` |
| `SYNA_LOG_LEVEL` | `debug`, `info`, `warn`, or `error` |
| `SYNA_ALLOW_HTTP` | development only, default `false` |

## Plain Docker Run

Example:

```bash
docker run -d \
  --name syna-server \
  --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -e SYNA_LISTEN=:8080 \
  -e SYNA_DATA_DIR=/var/lib/syna \
  -e SYNA_PUBLIC_BASE_URL=https://syna.example.com \
  -v /srv/syna:/var/lib/syna \
  ghcr.io/example/syna-server:latest serve
```

This intentionally binds only to localhost. Put a reverse proxy in front.

## Docker Compose

```yaml
services:
  syna-server:
    image: ghcr.io/example/syna-server:latest
    command: ["serve"]
    restart: unless-stopped
    environment:
      SYNA_LISTEN: ":8080"
      SYNA_DATA_DIR: "/var/lib/syna"
      SYNA_PUBLIC_BASE_URL: "https://syna.example.com"
      SYNA_SESSION_TTL: "24h"
      SYNA_EVENT_RETENTION: "24h"
      SYNA_ZERO_REF_RETENTION: "168h"
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - /srv/syna:/var/lib/syna
```

## Coolify Deployment

Coolify configuration requirements:

- resource type: Public Repository
- build pack: Dockerfile
- base directory: `/`
- Dockerfile location: `/deploy/docker/Dockerfile.server`
- Domains: the HTTPS URL clients use
- Ports Exposes: `8080`
- Port Mappings: empty
- Persistent Storage: Docker volume with name `syna-data`, empty source path,
  and destination path `/var/lib/syna`
- Healthcheck: enabled, path `/readyz`, port `8080` if Coolify shows a port
  field

No custom Docker options or separate WebSocket toggle are required when using
Coolify's normal domain/proxy path.

Do not store SQLite or object data in container-local ephemeral storage.
If using a bind mount instead of a Docker volume, mount a host directory such
as `/srv/syna` to `/var/lib/syna` and ensure it is writable by container UID
`10001`.

## Backup

Because payloads are end-to-end encrypted, offsite backups of the server volume are acceptable from a confidentiality perspective.

Recommended backup procedure:

1. stop the container, or run a SQLite-safe backup command
2. archive `/srv/syna`
3. store the archive remotely

Restore procedure:

1. restore `/srv/syna`
2. mount it back to `/var/lib/syna`
3. start the same or newer compatible `syna-server` image

## Upgrade Procedure

1. back up the mounted volume
2. pull the new image
3. stop the old container
4. start the new container
5. verify `/readyz`
6. optionally run `syna-server doctor`

Rolling upgrade is not required in v1.

## Hardening

Recommended server hardening:

- run the container as a non-root UID
- expose the backend only through HTTPS
- keep the raw backend port bound to localhost or a private interface
- keep the host volume owned by that service UID
- set container and host limits for memory and file descriptors
- monitor disk usage
- monitor WebSocket rejection and timeout logs
- restrict inbound ports to `80` and `443`
- keep the Docker image minimal

## Operational Warnings

- If the recovery key is lost on all clients, server backups remain encrypted but unreadable.
- If disk fills up, uploads will fail first and then live sync will degrade.
- If the reverse proxy does not support WebSockets, live sync will fall back to reconnect loops and appear broken.
- If a root is removed, its encrypted server-side history is deleted only after retention and GC; backups taken before that point may still contain old encrypted bytes.
- If two Syna server containers point at the same mounted volume, behavior is undefined and unsupported.
