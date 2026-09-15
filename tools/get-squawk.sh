#!/usr/bin/env bash
# Downloads the standalone squawk migration linter for the current platform into tools/squawk
# and verifies it against the SHA-256 pinned below. No npm/Node.
set -euo pipefail
VERSION="v2.65.0"
DIR="$(cd "$(dirname "$0")" && pwd)"
OUT="$DIR/squawk"
os="$(uname -s)"; arch="$(uname -m)"
case "$os-$arch" in
  Darwin-arm64)
    asset="squawk-darwin-arm64"
    sha256="05b140108aaa04404ed8a0e600dc83ab5c678cac929e621df6f08eb189bdfe5f" ;;
  Darwin-x86_64)
    asset="squawk-darwin-x64"
    sha256="5cb76d952443347a386cd9d2311ded3da7e31b932f81480cc00d6aa468121779" ;;
  Linux-aarch64|Linux-arm64)
    asset="squawk-linux-arm64"
    sha256="df11165f09629da80319e51434a6e4d2f27825801769879e15011d0091218bee" ;;
  Linux-x86_64)
    asset="squawk-linux-x64"
    sha256="9b7b2b9529a469647e32c6346e8d5a8a760857cbe2ee94ccb37f60e408babffb" ;;
  *) echo "unsupported platform: $os-$arch" >&2; exit 1 ;;
esac
url="https://github.com/sbdchd/squawk/releases/download/${VERSION}/${asset}"
echo "downloading $asset ($VERSION)"
tmp="$(mktemp "$DIR/.squawk.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
curl -fL -o "$tmp" "$url"
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$tmp" | cut -d' ' -f1)"
else
  got="$(shasum -a 256 "$tmp" | cut -d' ' -f1)"
fi
if [ "$got" != "$sha256" ]; then
  echo "checksum mismatch for $asset: expected $sha256, got $got" >&2
  exit 1
fi
chmod +x "$tmp"
mv "$tmp" "$OUT"
"$OUT" --version >/dev/null 2>&1 && echo "ready at $OUT ($("$OUT" --version))" || { echo "binary check failed" >&2; exit 1; }
