#!/usr/bin/env bash
set -euo pipefail

# FreeBSD's archived 14.3-RELEASE amd64 MANIFEST pins this immutable base.
url=https://archive.freebsd.org/old-releases/amd64/14.3-RELEASE/base.txz
sha256=e38b5cf756d60086a6c2f736eff19cc7685f7e2313e31d14342fc8df57200a92
if [[ "${1:-}" == --check ]]; then
  curl --fail --silent --show-error --head "$url" >/dev/null
  actual=$(curl --fail --silent --show-error "${url%/*}/MANIFEST" | awk '$1 == "base.txz" {print $2}')
  test "$actual" = "$sha256"
  exit
fi

: "${1:?Set the destination sysroot directory}"
archive=$(mktemp)
trap 'rm -f "$archive"' EXIT
curl --fail --silent --show-error "$url" -o "$archive"
echo "$sha256  $archive" | sha256sum --check
mkdir -p "$1"
tar -xJf "$archive" -C "$1" ./lib ./usr/include ./usr/lib
