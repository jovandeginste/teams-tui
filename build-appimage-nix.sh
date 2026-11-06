#!/usr/bin/env nix-shell
#!nix-shell -i bash -p asar appimage-run curl go gnumake
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CACHE_DIR="${XDG_CACHE_HOME:-${HOME}/.cache}/teams-tui"
APPIMAGETOOL_APPIMAGE="${CACHE_DIR}/appimagetool-x86_64.AppImage"

if ! command -v appimagetool >/dev/null 2>&1; then
  mkdir -p "${CACHE_DIR}"
  if [ ! -x "${APPIMAGETOOL_APPIMAGE}" ]; then
    echo "==> Downloading appimagetool-x86_64.AppImage..."
    curl -fL "https://github.com/AppImage/appimagetool/releases/download/continuous/appimagetool-x86_64.AppImage" \
      -o "${APPIMAGETOOL_APPIMAGE}"
    chmod +x "${APPIMAGETOOL_APPIMAGE}"
  fi

  WRAPPER_DIR="$(mktemp -d)"
  trap 'rm -rf "${WRAPPER_DIR}"' EXIT

  cat <<EOF > "${WRAPPER_DIR}/appimagetool"
#!/usr/bin/env bash
exec appimage-run "${APPIMAGETOOL_APPIMAGE}" "\$@"
EOF
  chmod +x "${WRAPPER_DIR}/appimagetool"
  export PATH="${WRAPPER_DIR}:${PATH}"
fi

"${ROOT_DIR}/build-appimage.sh" "$@"
