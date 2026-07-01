#!/bin/sh
# Build the sideshow-agent release artifacts: the three node arches + a checksum
# manifest + the installer, ready to upload to any static host (GitHub Releases,
# S3, nginx, …). Pure-Go static binaries — no CGO, no per-arch toolchain.
#
#   sh scripts/build-release.sh              # → agent/dist/  (VERSION = short git sha)
#   VERSION=v0.1.0 sh scripts/build-release.sh
#
# Output (agent/dist/):
#   sideshow-agent-arm64   (Raspberry Pi 64-bit / any arm64 Debian)
#   sideshow-agent-amd64   (x86-64 mini-PC)
#   sideshow-agent-armhf   (Raspberry Pi 32-bit, arm/v7)
#   install.sh             (a copy of the installer, so it can be an asset too)
#   SHA256SUMS             (sha256 of all of the above — install.sh verifies against it)
set -eu

HERE="$(cd "$(dirname "$0")/.." && pwd)"   # the agent/ dir
DIST="$HERE/dist"
VERSION="${VERSION:-$(git -C "$HERE" rev-parse --short HEAD 2>/dev/null || echo dev)}"

# GOARCH/GOARM per Debian arch label.
build_one() {
  label="$1"; goarch="$2"; goarm="${3:-}"
  out="$DIST/sideshow-agent-$label"
  echo ">> building $label (GOARCH=$goarch${goarm:+ GOARM=$goarm})"
  ( cd "$HERE" && \
    GOOS=linux GOARCH="$goarch" GOARM="$goarm" CGO_ENABLED=0 \
      go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out" . )
}

mkdir -p "$DIST"
rm -f "$DIST"/sideshow-agent-* "$DIST/SHA256SUMS"

build_one arm64 arm64
build_one amd64 amd64
build_one armhf arm 7

cp "$HERE/scripts/install.sh" "$DIST/install.sh"

echo ">> writing SHA256SUMS"
( cd "$DIST" && \
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum sideshow-agent-arm64 sideshow-agent-amd64 sideshow-agent-armhf install.sh > SHA256SUMS
  else
    # macOS: shasum -a 256 emits the same "<hash>  <file>" format
    shasum -a 256 sideshow-agent-arm64 sideshow-agent-amd64 sideshow-agent-armhf install.sh > SHA256SUMS
  fi )

echo ""
echo "=== release $VERSION in $DIST ==="
ls -lh "$DIST"/sideshow-agent-* "$DIST/install.sh" | awk '{print "  "$5"\t"$NF}'
echo "--- SHA256SUMS ---"
sed 's/^/  /' "$DIST/SHA256SUMS"
echo ""
echo "Next: upload these to your host, then run the one-liner (see scripts/install.sh)."
