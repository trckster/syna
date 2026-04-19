#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COVER_DIR="${ROOT_DIR}/coverage"
COVER_PROFILE="${COVER_PROFILE:-${COVER_DIR}/coverage.out}"

mkdir -p "${COVER_DIR}"

(
  cd "${ROOT_DIR}"
  mapfile -t packages < <(go list -f '{{if .TestGoFiles}}{{.ImportPath}}{{end}}' ./... | sed '/^$/d')
  if [[ "${#packages[@]}" -eq 0 ]]; then
    echo "no test packages found" >&2
    exit 1
  fi
  GOTOOLCHAIN="${GOTOOLCHAIN:-auto}" go test -covermode=atomic -coverprofile="${COVER_PROFILE}" "${packages[@]}"
  go tool cover -func="${COVER_PROFILE}"
)
