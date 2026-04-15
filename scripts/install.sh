#!/usr/bin/env bash
set -euo pipefail

REPO="diggerhq/opencomputer"
BIN_NAME="oc"
INSTALL_DIR="${OC_INSTALL_DIR:-$HOME/.local/bin}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) echo "Unsupported architecture: $arch" >&2; exit 1 ;;
esac

case "$os" in
  linux|darwin) ;;
  *) echo "Unsupported OS: $os" >&2; exit 1 ;;
esac

url="https://github.com/${REPO}/releases/latest/download/${BIN_NAME}-${os}-${arch}"
target="${INSTALL_DIR}/${BIN_NAME}"

mkdir -p "$INSTALL_DIR"
echo "Downloading $url"
curl -fsSL "$url" -o "$target"
chmod +x "$target"

echo "Installed ${BIN_NAME} to ${target}"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    echo
    echo "warning: $INSTALL_DIR is not on your PATH."
    echo "Add it by appending this line to your shell rc file (~/.bashrc, ~/.zshrc):"
    echo "  export PATH=\"$INSTALL_DIR:\$PATH\""
    ;;
esac
