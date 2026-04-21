# Release Checklist

Run this checklist before publishing a server or client build.

1. Build and test with Go `1.25.9` or a newer fixed `1.25.x` patch release.
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
   - `govulncheck`
   - Docker with Compose v2
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
16. Verify `SYNA_PUBLIC_BASE_URL` is `https://...` in production and that `SYNA_ALLOW_HTTP` is not enabled.
17. Verify the backend is reachable only through an HTTPS reverse proxy and the raw backend port is not publicly exposed.
18. Verify object-store disk usage, free space, and file-descriptor limits are within operating thresholds before rollout.
