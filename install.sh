#!/usr/bin/env bash
#
# sshmng one-click install script (macOS / Linux).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/jim58246/sshmng/main/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/jim58246/sshmng/main/install.sh | bash -s -- --yes
#
# Flags:
#   --yes               Also run 'sshmng install --yes' after placing the binary
#   --install-dir <p>   Override install directory (default: /usr/local/bin if
#                       writable, else ~/.local/bin)
#
# Windows is not supported by this script — download the .zip from
# https://github.com/jim58246/sshmng/releases and extract manually, or follow
# docs/agent-install-prompt.md to have your AI Agent do it.

set -euo pipefail

OWNER="jim58246"
REPO="sshmng"

RUN_INSTALL=false
INSTALL_DIR=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --yes) RUN_INSTALL=true; shift ;;
    --install-dir) INSTALL_DIR="${2:?--install-dir requires a path}"; shift 2 ;;
    --help|-h)
      cat <<'HELP'
Usage: install.sh [--yes] [--install-dir <path>]
  --yes               Also run 'sshmng install --yes' after placing the binary
  --install-dir <p>   Override install directory (default: /usr/local/bin if
                      writable, else ~/.local/bin)
HELP
      exit 0
      ;;
    *) echo "Unknown arg: $1 (use --help)" >&2; exit 1 ;;
  esac
done

# --- detect platform ---
OS="$(uname -s)"
ARCH="$(uname -m)"
case "$OS" in
  Darwin) OS="darwin" ;;
  Linux)  OS="linux" ;;
  *)
    echo "Unsupported OS: $OS (this script supports macOS and Linux;" \
         "Windows users please download the release .zip directly)" >&2
    exit 1
    ;;
esac
case "$ARCH" in
  x86_64|amd64)  ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

echo ">> Platform: $OS/$ARCH"

# --- fetch latest version ---
echo ">> Fetching latest release..."
LATEST="$(curl -fsSL "https://api.github.com/repos/${OWNER}/${REPO}/releases/latest" \
          | grep -oE '"tag_name":[[:space:]]*"[^"]+"' | head -1 | cut -d'"' -f4)"
if [[ -z "${LATEST:-}" ]]; then
  echo "Failed to fetch latest version from GitHub API" >&2
  exit 1
fi
echo ">> Latest release: $LATEST"

# --- download archive ---
ARCHIVE="sshmng-${LATEST}-${OS}-${ARCH}.tar.gz"
URL="https://github.com/${OWNER}/${REPO}/releases/download/${LATEST}/${ARCHIVE}"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT
echo ">> Downloading $URL"
curl -fsSL -o "$TMPDIR/$ARCHIVE" "$URL"

# --- extract ---
echo ">> Extracting..."
tar -xzf "$TMPDIR/$ARCHIVE" -C "$TMPDIR"
if [[ ! -x "$TMPDIR/sshmng" ]]; then
  echo "Binary 'sshmng' not found in archive" >&2
  exit 1
fi

# --- pick install dir ---
if [[ -z "$INSTALL_DIR" ]]; then
  if [[ -w /usr/local/bin ]]; then
    INSTALL_DIR="/usr/local/bin"
  else
    INSTALL_DIR="$HOME/.local/bin"
  fi
fi
mkdir -p "$INSTALL_DIR"

# --- place binary ---
TARGET="$INSTALL_DIR/sshmng"
mv "$TMPDIR/sshmng" "$TARGET"
chmod +x "$TARGET"
echo ">> Installed: $TARGET"

# --- macOS quarantine (curl downloads don't set it, but be safe) ---
if [[ "$OS" == "darwin" ]] && xattr "$TARGET" 2>/dev/null | grep -q "com.apple.quarantine"; then
  xattr -d com.apple.quarantine "$TARGET" 2>/dev/null || true
  echo ">> Removed macOS quarantine attribute"
fi

# --- PATH check ---
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    echo ""
    echo "WARNING: $INSTALL_DIR is not on your PATH."
    echo "Add this to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
    echo "  export PATH=\"$INSTALL_DIR:\$PATH\""
    ;;
esac

# --- optional: run install ---
if [[ "$RUN_INSTALL" == "true" ]]; then
  echo ""
  echo ">> Running 'sshmng install --yes'..."
  "$TARGET" install --yes
else
  echo ""
  echo "Next steps:"
  echo "  1. sshmng install        # create ~/.sshmng/ + inject into AI Agents"
  echo "  2. sshmng doctor         # verify setup"
  echo "  3. Restart your AI Agent"
  echo ""
  echo "Tip: re-run with --yes to auto-run 'sshmng install --yes'."
fi

echo ""
echo "Done."
