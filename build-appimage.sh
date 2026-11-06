#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERSION="${V:-2.28.0}"
OUTPUT=""

usage() {
  cat <<EOF
Usage: $(basename "$0") [-V <version>] [-o <output-path>]

Build a custom teams-for-linux AppImage with message-logger.js and teams-worker bundled.

Options:
  -V <version>      Upstream teams-for-linux version (default: ${VERSION}, or env V)
  -o <output-path>  Output AppImage path (default: bin/teams-for-linux-<version>-logger.AppImage)
  -h                Show this help message

Examples:
  ./$(basename "$0") -V 2.28.0
  V=2.28.0 ./$(basename "$0")
  make appimage V=2.28.0
EOF
}

while getopts ":V:o:h" opt; do
  case "${opt}" in
    V)
      VERSION="${OPTARG}"
      ;;
    o)
      OUTPUT="${OPTARG}"
      ;;
    h)
      usage
      exit 0
      ;;
    :)
      echo "Error: -${OPTARG} requires an argument." >&2
      usage >&2
      exit 1
      ;;
    \?)
      echo "Error: invalid option -${OPTARG}" >&2
      usage >&2
      exit 1
      ;;
  esac
done

VERSION="${VERSION#v}"

if [ -z "${OUTPUT}" ]; then
  OUTPUT="${ROOT_DIR}/bin/teams-for-linux-${VERSION}-logger.AppImage"
elif [[ "${OUTPUT}" != /* ]]; then
  OUTPUT="$(pwd)/${OUTPUT}"
fi

if command -v asar >/dev/null 2>&1; then
  ASAR_CMD=(asar)
elif command -v npx >/dev/null 2>&1; then
  ASAR_CMD=(npx --yes @electron/asar)
else
  echo "Error: neither 'asar' nor 'npx' found in PATH." >&2
  exit 1
fi

if ! command -v appimagetool >/dev/null 2>&1; then
  echo "Error: 'appimagetool' not found in PATH." >&2
  exit 1
fi

echo "==> Building teams-worker..."
make -C "${ROOT_DIR}" build

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

UPSTREAM_APPIMAGE="${WORKDIR}/teams-for-linux-${VERSION}.AppImage"
UPSTREAM_URL="https://github.com/IsmaelMartinez/teams-for-linux/releases/download/v${VERSION}/teams-for-linux-${VERSION}.AppImage"

echo "==> Downloading upstream teams-for-linux v${VERSION}..."
curl -fL "${UPSTREAM_URL}" -o "${UPSTREAM_APPIMAGE}"
chmod +x "${UPSTREAM_APPIMAGE}"

APPDIR="${WORKDIR}/squashfs-root"

echo "==> Extracting upstream AppImage..."
if command -v appimage-run >/dev/null 2>&1; then
  appimage-run -x "${APPDIR}" "${UPSTREAM_APPIMAGE}" >/dev/null
else
  (
    cd "${WORKDIR}"
    "${UPSTREAM_APPIMAGE}" --appimage-extract >/dev/null
  )
fi

echo "==> Extracting resources/app.asar..."
"${ASAR_CMD[@]}" extract "${APPDIR}/resources/app.asar" "${APPDIR}/resources/app"
rm -f "${APPDIR}/resources/app.asar"

echo "==> Injecting message-logger.js and teams-worker..."
cp "${ROOT_DIR}/message-logger.js" "${APPDIR}/resources/app/app/browser/message-logger.js"
echo 'require("./message-logger.js");' >> "${APPDIR}/resources/app/app/browser/preload.js"
mv "${APPDIR}/resources/app/app/menus/appMenu.js" "${APPDIR}/resources/app/app/menus/appMenu.orig.js"
echo 'module.exports = require("../browser/message-logger.js");' > "${APPDIR}/resources/app/app/menus/appMenu.js"

cp "${ROOT_DIR}/bin/teams-worker" "${APPDIR}/teams-worker"
chmod +x "${APPDIR}/teams-worker"

echo "==> Repacking resources/app.asar..."
"${ASAR_CMD[@]}" pack "${APPDIR}/resources/app" "${APPDIR}/resources/app.asar" \
  --unpack-dir "node_modules/@cbor-extract"
rm -rf "${APPDIR}/resources/app"

mkdir -p "$(dirname "${OUTPUT}")"
echo "==> Building AppImage: ${OUTPUT}"
ARCH=x86_64 appimagetool "${APPDIR}" "${OUTPUT}"

echo "==> Done: ${OUTPUT}"
