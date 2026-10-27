# Release Checklist

Run this checklist before publishing a server or client build.

1. Build and test with Go `1.27.3`, matching `go.mod` and the server build image.
2. Run `go test ./...`.
3. Run `go vet ./...`.
4. Run `govulncheck ./...` and block the release on any reachable vulnerability.
5. Run `./scripts/coverage.sh` and keep the generated profile as a release artifact when useful.
6. Run `docker build -f deploy/docker/Dockerfile.server .`.
7. Run `docker compose -f deploy/docker/compose.example.yml config`.
8. Run `CHECK_DEPS=true ./scripts/release.sh` before the archive build.
9. Verify release host packages are installed:
   - `gcc` or Debian/Ubuntu `build-essential`
   - `aarch64-linux-gnu-gcc` or Debian/Ubuntu `gcc-aarch64-linux-gnu`
   - `govulncheck` (`go install golang.org/x/vuln/cmd/govulncheck@v1.9.0`)
   - Docker with the Compose plugin (`docker compose`)
10. Run `./scripts/release.sh` and inspect the generated `dist/*.tar.gz` archives.
11. Run `./scripts/smoke.sh` and record the result.
12. Run a Docker runtime check:
   - start a container with a mounted temporary data volume
   - verify `/readyz`
   - run `syna-server doctor` against the same volume
   - restart the container and verify data remains available
13. Verify first-client and second-client flows against the release binaries.
14. Upload the Linux release archives to the GitHub Release for the version tag:
   - `syna-<version>-linux-amd64.tar.gz`
   - `syna-<version>-linux-arm64.tar.gz`
15. Verify the latest GitHub Release exposes those archives and does not require
    client users to build from source.
16. Verify the one-line client installer resolves the latest version and installs
    the matching client binary:
    `curl -fsSL https://raw.githubusercontent.com/trckster/syna/master/scripts/install.sh | sh`
17. Verify `SYNA_PUBLIC_BASE_URL` is `https://...` in production and that `SYNA_ALLOW_HTTP` is not enabled.
18. Verify the backend is reachable only through an HTTPS reverse proxy and the raw backend port is not publicly exposed.
19. Verify object-store disk usage, free space, and file-descriptor limits are within operating thresholds before rollout.

## Client artifacts and server deployment

CI builds Linux amd64 and arm64 archives with CGO enabled on Ubuntu 24.04,
retaining that glibc baseline for client compatibility. Both C compilers listed
above are required; the SQLite driver bundles SQLite, so client hosts do not
need a separate SQLite development package. `scripts/release.sh` builds local
archives only. It does not create tags, publish a GitHub Release, or deploy a
server. Set `VERSION`, `COMMIT`, and `SOURCE_DATE_EPOCH` to fixed values when
rebuilding reproducible archives from the same source and toolchain.

Publishing those archives in a GitHub Release makes them available to the client
installer. Coolify deploys the server separately from `master`, using
`deploy/docker/Dockerfile.server` and its persistent `/var/lib/syna` volume.
Publishing client artifacts does not update a running server; rebuilding the
Coolify server does not update installed clients. Follow the server backup and
upgrade procedure in [`deploy/README.md`](../deploy/README.md), then verify the
running server revision, `/readyz`, and a two-client sync flow. Update clients
through the installer and verify their daemon build with `syna status`.

The Docker base images and CI actions are pinned to immutable digests/commits.
When refreshing them, resolve the new version's digest/commit from the official
registry/repository and update the version label with the pin. Alpine packages
are resolved from the pinned image's stable 3.24 repositories during each build
so current security fixes remain available.
