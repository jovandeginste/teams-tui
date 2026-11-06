#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

WRAPPER="$(command -v teams-for-linux || true)"
if [ -z "${WRAPPER}" ]; then
  echo "teams-for-linux not found in PATH" >&2
  exit 1
fi

if [ ! -x "${ROOT_DIR}/bin/teams-worker" ]; then
  make -C "${ROOT_DIR}" build
fi

if pgrep -x teams-for-linux >/dev/null 2>&1; then
  echo "Closing existing teams-for-linux instance first..."
  pkill -x teams-for-linux || true
  sleep 1
fi

EXTRACTED="$(sed -n 's/.*appimage-run -w \([^ ]*\).*/\1/p' "${WRAPPER}" | head -n1)"
if [ -z "${EXTRACTED}" ] || [ ! -d "${EXTRACTED}" ]; then
  echo "Could not locate extracted AppDir in ${WRAPPER}" >&2
  exit 1
fi

export TEAMS_LOGGER_WORKER="${ROOT_DIR}/bin/teams-worker"
export TEAMS_LOGGER_SCRIPT="${ROOT_DIR}/message-logger.js"

echo "Starting teams-for-linux with live ${ROOT_DIR}/message-logger.js and ${ROOT_DIR}/bin/teams-worker ..."
sed "s|--bind /nix /nix|--bind /nix /nix --ro-bind \"${ROOT_DIR}/message-logger.js\" \"${EXTRACTED}/resources/app/app/browser/message-logger.js\"|" "$(command -v appimage-run)" | bash -s -- -w "${EXTRACTED}" -- "$@"
