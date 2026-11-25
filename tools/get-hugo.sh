#!/usr/bin/env bash
# Downloads the standard (non-extended) Hugo binary for the current platform into tools/hugo
# and verifies the release archive against the SHA-256 pinned below (taken from the release's
# hugo_<version>_checksums.txt). Builds the landing site in site/. No npm/Node, no global install.
set -euo pipefail
VERSION="0.166.0"
DIR="$(cd "$(dirname "$0")" && pwd)"
OUT="$DIR/hugo"
os="$(uname -s)"; arch="$(uname -m)"
case "$os-$arch" in
  Darwin-arm64|Darwin-x86_64)
    # macOS ships only as a universal installer package; it is unpacked, never installed.
    asset="hugo_${VERSION}_darwin-universal.pkg"
    sha256="387c90d4cc3fa4add9949db0304c1db52175a2e939a77353e2809da15722b5a6" ;;
  Linux-aarch64|Linux-arm64)
    asset="hugo_${VERSION}_linux-arm64.tar.gz"
    sha256="0e15cbc595e799401698c11af39d2594b64292b39d9ec7a19665bd43e05fcb2c" ;;
  Linux-x86_64)
    asset="hugo_${VERSION}_linux-amd64.tar.gz"
    sha256="45228f5a52eb118b0ca168068f01d7df0447314a24056f1d29667ed9fc368308" ;;
  *) echo "unsupported platform: $os-$arch" >&2; exit 1 ;;
esac
url="https://github.com/gohugoio/hugo/releases/download/v${VERSION}/${asset}"
echo "downloading $asset"
tmp="$(mktemp -d "$DIR/.hugo.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
curl -fL -o "$tmp/$asset" "$url"
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$tmp/$asset" | cut -d' ' -f1)"
else
  got="$(shasum -a 256 "$tmp/$asset" | cut -d' ' -f1)"
fi
if [ "$got" != "$sha256" ]; then
  echo "checksum mismatch for $asset: expected $sha256, got $got" >&2
  exit 1
fi
case "$asset" in
  *.pkg)
    pkgutil --expand-full "$tmp/$asset" "$tmp/x"
    bin="$(find "$tmp/x" -type f -name hugo -perm -u+x | head -n 1)" ;;
  *.tar.gz)
    mkdir "$tmp/x"
    tar -xzf "$tmp/$asset" -C "$tmp/x" hugo
    bin="$tmp/x/hugo" ;;
esac
[ -n "$bin" ] && [ -f "$bin" ] || { echo "hugo binary not found in $asset" >&2; exit 1; }
chmod +x "$bin"
mv "$bin" "$OUT"
"$OUT" version >/dev/null 2>&1 && echo "ready at $OUT ($("$OUT" version))" || { echo "binary check failed" >&2; exit 1; }
