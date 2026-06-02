#!/usr/bin/env bash
# Downloads the standalone Tailwind CSS v4 CLI for the current platform into tools/tailwindcss. No npm/Node.
set -euo pipefail
VERSION="v4.3.0"
DIR="$(cd "$(dirname "$0")" && pwd)"
OUT="$DIR/tailwindcss"
os="$(uname -s)"; arch="$(uname -m)"
case "$os-$arch" in
  Darwin-arm64)  asset="tailwindcss-macos-arm64" ;;
  Darwin-x86_64) asset="tailwindcss-macos-x64" ;;
  Linux-aarch64|Linux-arm64) asset="tailwindcss-linux-arm64" ;;
  Linux-x86_64)  asset="tailwindcss-linux-x64" ;;
  *) echo "unsupported platform: $os-$arch" >&2; exit 1 ;;
esac
url="https://github.com/tailwindlabs/tailwindcss/releases/download/${VERSION}/${asset}"
echo "downloading $asset ($VERSION)"
curl -fL -o "$OUT" "$url"
chmod +x "$OUT"
"$OUT" --help >/dev/null 2>&1 && echo "ready at $OUT" || { echo "binary check failed" >&2; exit 1; }
