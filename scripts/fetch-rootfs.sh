#!/bin/sh
# Download the Alpine minirootfs used by tests and benchmarks and check its hash.
set -eu
VERSION=3.24.4
SHA256=cdaa38cb8de3537967ee5b6c9b6cd00cc962da2b018e6eea48696bb274923012
DEST=${1:-rootfs}
URL="https://dl-cdn.alpinelinux.org/alpine/v${VERSION%.*}/releases/x86_64/alpine-minirootfs-${VERSION}-x86_64.tar.gz"

if [ -f "$DEST/etc/alpine-release" ] && [ "$(cat "$DEST/etc/alpine-release")" = "$VERSION" ]; then
	echo "$DEST already has Alpine $VERSION"
	exit 0
fi
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
curl -fsSL "$URL" -o "$tmp"
echo "$SHA256  $tmp" | sha256sum -c - >/dev/null
mkdir -p "$DEST"
tar -xzf "$tmp" -C "$DEST"
echo "Alpine $VERSION in $DEST"
