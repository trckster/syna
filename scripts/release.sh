#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${ROOT_DIR}/dist"
VERSION="${VERSION:-dev}"
COMMIT="${COMMIT:-$(git -C "${ROOT_DIR}" rev-parse --short HEAD 2>/dev/null || echo unknown)}"
ARCHES="${ARCHES:-amd64 arm64}"
CHECK_DEPS="${CHECK_DEPS:-false}"

if [[ -n "${SOURCE_DATE_EPOCH:-}" ]]; then
  BUILD_DATE="$(date -u -d "@${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ)"
else
  BUILD_DATE="${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
fi

mkdir -p "${DIST_DIR}"
rm -f "${DIST_DIR}"/syna-*.tar.gz

build_archive() {
  local goarch="$1"
  local package_dir="${DIST_DIR}/syna-${VERSION}-linux-${goarch}"
  local tarball="${DIST_DIR}/syna-${VERSION}-linux-${goarch}.tar.gz"
  local ldflags="-buildid= -X syna/internal/buildinfo.Version=${VERSION} -X syna/internal/buildinfo.Commit=${COMMIT} -X syna/internal/buildinfo.BuildDate=${BUILD_DATE}"
  local cc="gcc"

  if [[ "${goarch}" == "arm64" ]]; then
    cc="aarch64-linux-gnu-gcc"
  fi
  if ! command -v "${cc}" >/dev/null 2>&1; then
    case "${cc}" in
      gcc)
        echo "missing required C compiler: gcc (Debian/Ubuntu: sudo apt-get install build-essential)" >&2
        ;;
      aarch64-linux-gnu-gcc)
        echo "missing required C compiler: aarch64-linux-gnu-gcc (Debian/Ubuntu: sudo apt-get install gcc-aarch64-linux-gnu)" >&2
        ;;
      *)
        echo "missing required C compiler: ${cc}" >&2
        ;;
    esac
    exit 1
  fi

  rm -rf "${package_dir}"
  mkdir -p "${package_dir}"

  (
    cd "${ROOT_DIR}"
    CC="${cc}" CGO_ENABLED=1 GOOS=linux GOARCH="${goarch}" \
      go build -trimpath -buildvcs=false -ldflags "${ldflags}" -o "${package_dir}/syna" ./cmd/syna
    CC="${cc}" CGO_ENABLED=1 GOOS=linux GOARCH="${goarch}" \
      go build -trimpath -buildvcs=false -ldflags "${ldflags}" -o "${package_dir}/syna-server" ./cmd/syna-server
  )

  cp "${ROOT_DIR}/README.md" "${package_dir}/README.md"

  tar \
    --sort=name \
    --mtime="${BUILD_DATE}" \
    --owner=0 \
    --group=0 \
    --numeric-owner \
    -C "${DIST_DIR}" \
    -czf "${tarball}" \
    "$(basename "${package_dir}")"

  rm -rf "${package_dir}"
}

for arch in ${ARCHES}; do
  case "${arch}" in
    amd64|arm64)
      if [[ "${CHECK_DEPS}" == "true" ]]; then
        cc="gcc"
        if [[ "${arch}" == "arm64" ]]; then
          cc="aarch64-linux-gnu-gcc"
        fi
        command -v "${cc}" >/dev/null 2>&1 || {
          echo "missing required C compiler for ${arch}: ${cc}" >&2
          exit 1
        }
      else
        build_archive "${arch}"
      fi
      ;;
    *)
      echo "unsupported ARCHES entry: ${arch}" >&2
      exit 1
      ;;
  esac
done

if [[ "${CHECK_DEPS}" == "true" ]]; then
  echo "release dependencies available for ARCHES=${ARCHES}"
fi
