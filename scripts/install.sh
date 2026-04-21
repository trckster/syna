#!/usr/bin/env sh
set -eu

REPO="${SYNA_REPO:-trckster/syna}"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

log() {
  printf '%s\n' "$*"
}

warn() {
  printf 'warning: %s\n' "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

detect_arch() {
  case "$(uname -m)" in
    x86_64 | amd64)
      printf '%s\n' "amd64"
      ;;
    aarch64 | arm64)
      printf '%s\n' "arm64"
      ;;
    *)
      die "unsupported CPU architecture: $(uname -m); Syna publishes linux-amd64 and linux-arm64 client archives"
      ;;
  esac
}

detect_version() {
  latest_url="https://github.com/${REPO}/releases/latest"
  if ! effective_url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "${latest_url}")"; then
    die "could not reach ${latest_url}"
  fi
  version="${effective_url##*/}"
  if [ -z "${version}" ] || [ "${version}" = "latest" ]; then
    die "could not determine latest release version from ${latest_url}"
  fi
  printf '%s\n' "${version}"
}

download() {
  url="$1"
  output="$2"
  curl -fsSL --retry 3 --retry-delay 1 -o "${output}" "${url}"
}

install_binary() {
  binary="$1"
  target="${INSTALL_DIR}/syna"

  if install -d "${INSTALL_DIR}" 2>/dev/null && install -m 0755 "${binary}" "${target}" 2>/dev/null; then
    return 0
  fi

  if command -v sudo >/dev/null 2>&1; then
    sudo install -d "${INSTALL_DIR}"
    sudo install -m 0755 "${binary}" "${target}"
    return 0
  fi

  die "cannot write to ${INSTALL_DIR}; rerun with sudo or set INSTALL_DIR to a writable directory"
}

restart_user_service_if_present() {
  if command -v systemctl >/dev/null 2>&1 && systemctl --user cat syna.service >/dev/null 2>&1; then
    if ! systemctl --user daemon-reload; then
      warn "installed new binary, but could not reload user systemd"
    fi
    if systemctl --user restart syna.service; then
      log "restarted syna user service"
    else
      warn "installed new binary, but could not restart syna user service"
    fi
  fi
}

main() {
  if [ "$(uname -s)" != "Linux" ]; then
    die "Syna client releases currently support Linux only"
  fi

  need_cmd curl
  need_cmd install
  need_cmd mktemp
  need_cmd tar
  need_cmd uname

  arch="$(detect_arch)"
  version="${SYNA_VERSION:-${VERSION:-}}"
  if [ -z "${version}" ]; then
    version="$(detect_version)"
  fi

  archive="syna-${version}-linux-${arch}.tar.gz"
  package_dir="syna-${version}-linux-${arch}"
  base_url="https://github.com/${REPO}/releases/download/${version}"
  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "${tmp_dir}"' EXIT INT TERM

  log "installing Syna ${version} for linux-${arch}"
  download "${base_url}/${archive}" "${tmp_dir}/${archive}"

  tar -xzf "${tmp_dir}/${archive}" -C "${tmp_dir}"
  binary="${tmp_dir}/${package_dir}/syna"
  if [ ! -f "${binary}" ]; then
    die "release archive did not contain ${package_dir}/syna"
  fi

  install_binary "${binary}"
  restart_user_service_if_present

  log "installed ${INSTALL_DIR}/syna"
  "${INSTALL_DIR}/syna" version
}

main "$@"
