#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"
PORT="${SYNA_SMOKE_PORT:-18080}"
SERVER_URL="http://127.0.0.1:${PORT}"

cleanup() {
  if [[ -n "${SERVER_PID:-}" ]]; then kill "${SERVER_PID}" 2>/dev/null || true; fi
  if [[ -n "${DAEMON_A_PID:-}" ]]; then kill "${DAEMON_A_PID}" 2>/dev/null || true; fi
  if [[ -n "${DAEMON_B_PID:-}" ]]; then kill "${DAEMON_B_PID}" 2>/dev/null || true; fi
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

mkdir -p "${TMP_DIR}/bin"

(
  cd "${ROOT_DIR}"
  go build -o "${TMP_DIR}/bin/syna" ./cmd/syna
  go build -o "${TMP_DIR}/bin/syna-server" ./cmd/syna-server
)

export SYNA_ALLOW_HTTP=true
SYNA_LISTEN="127.0.0.1:${PORT}" \
SYNA_DATA_DIR="${TMP_DIR}/server" \
SYNA_PUBLIC_BASE_URL="${SERVER_URL}" \
  "${TMP_DIR}/bin/syna-server" serve >"${TMP_DIR}/server.log" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 50); do
  if curl -fsS "${SERVER_URL}/readyz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done
curl -fsS "${SERVER_URL}/readyz" >/dev/null

HOME_A="${TMP_DIR}/home-a"
HOME_B="${TMP_DIR}/home-b"
mkdir -p "${HOME_A}/notes" "${HOME_B}"
printf 'initial\n' >"${HOME_A}/notes/note.txt"

env HOME="${HOME_A}" XDG_CONFIG_HOME="${HOME_A}/.config" XDG_STATE_HOME="${HOME_A}/.local/state" SYNA_ALLOW_HTTP=true \
  "${TMP_DIR}/bin/syna" daemon >"${TMP_DIR}/daemon-a.log" 2>&1 &
DAEMON_A_PID=$!
env HOME="${HOME_B}" XDG_CONFIG_HOME="${HOME_B}/.config" XDG_STATE_HOME="${HOME_B}/.local/state" SYNA_ALLOW_HTTP=true \
  "${TMP_DIR}/bin/syna" daemon >"${TMP_DIR}/daemon-b.log" 2>&1 &
DAEMON_B_PID=$!

for socket in "${HOME_A}/.local/state/syna/agent.sock" "${HOME_B}/.local/state/syna/agent.sock"; do
  for _ in $(seq 1 50); do
    if [[ -S "${socket}" ]]; then
      break
    fi
    sleep 0.1
  done
  [[ -S "${socket}" ]]
done

CONNECT_OUT="$(printf '\n' | env HOME="${HOME_A}" XDG_CONFIG_HOME="${HOME_A}/.config" XDG_STATE_HOME="${HOME_A}/.local/state" SYNA_ALLOW_HTTP=true \
  "${TMP_DIR}/bin/syna" connect "${SERVER_URL}")"
RECOVERY_KEY="$(printf '%s\n' "${CONNECT_OUT}" | grep -Eo 'syna1-[0-9a-f]{64}-[0-9a-f]{8}' | head -n1)"
[[ -n "${RECOVERY_KEY}" ]]

env HOME="${HOME_A}" XDG_CONFIG_HOME="${HOME_A}/.config" XDG_STATE_HOME="${HOME_A}/.local/state" SYNA_ALLOW_HTTP=true \
  "${TMP_DIR}/bin/syna" add "${HOME_A}/notes"

printf '%s\n' "${RECOVERY_KEY}" | env HOME="${HOME_B}" XDG_CONFIG_HOME="${HOME_B}/.config" XDG_STATE_HOME="${HOME_B}/.local/state" SYNA_ALLOW_HTTP=true \
  "${TMP_DIR}/bin/syna" connect "${SERVER_URL}" >/dev/null

for _ in $(seq 1 100); do
  if [[ -f "${HOME_B}/notes/note.txt" ]] && [[ "$(cat "${HOME_B}/notes/note.txt")" == "initial" ]]; then
    break
  fi
  sleep 0.1
done
[[ "$(cat "${HOME_B}/notes/note.txt")" == "initial" ]]

printf 'edited\n' >"${HOME_A}/notes/note.txt"
for _ in $(seq 1 150); do
  if [[ "$(cat "${HOME_B}/notes/note.txt" 2>/dev/null || true)" == "edited" ]]; then
    break
  fi
  sleep 0.1
done
[[ "$(cat "${HOME_B}/notes/note.txt")" == "edited" ]]

env HOME="${HOME_A}" XDG_CONFIG_HOME="${HOME_A}/.config" XDG_STATE_HOME="${HOME_A}/.local/state" SYNA_ALLOW_HTTP=true \
  "${TMP_DIR}/bin/syna" disconnect

echo "smoke workflow passed"
