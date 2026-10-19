#!/usr/bin/env bash
set -euo pipefail

# FreeBSD's archived 14.3-RELEASE amd64 MANIFEST pins this immutable base. The
# pinned checksum is the only trust anchor: cache hits and every source below,
# including third-party mirrors, are used only after matching it.
sha256=e38b5cf756d60086a6c2f736eff19cc7685f7e2313e31d14342fc8df57200a92
urls=(
  https://archive.freebsd.org/old-releases/amd64/14.3-RELEASE/base.txz
  https://ftp.jaist.ac.jp/pub/FreeBSD/releases/amd64/14.3-RELEASE/base.txz
  https://mirror.math.princeton.edu/pub/FreeBSD/releases/amd64/14.3-RELEASE/base.txz
)
cache=${FREEBSD_SYSROOT_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/cpa-release/freebsd}
archive=$cache/base-$sha256.txz

verified() { echo "$sha256  $1" | sha256sum --check --status; }

if [[ -f "$archive" ]] && verified "$archive"; then
  echo "freebsd-sysroot: verified cached base.txz" >&2
else
  rm -f "$archive"
  mkdir -p "$cache"
  part=$(mktemp "$cache/.base.XXXXXX")
  trap 'rm -f "$part"' EXIT
  for url in "${urls[@]}"; do
    if curl --fail --silent --show-error --location --connect-timeout 20 "$url" -o "$part" && verified "$part"; then
      mv "$part" "$archive"
      echo "freebsd-sysroot: cached base.txz verified from $url" >&2
      break
    fi
    echo "freebsd-sysroot: $url unavailable or checksum mismatch" >&2
  done
  [[ -f "$archive" ]] || { echo "freebsd-sysroot: no source matched the pinned checksum" >&2; exit 1; }
fi

[[ "${1:-}" == --check ]] && exit
: "${1:?Set the destination sysroot directory}"
mkdir -p "$1"
tar -xJf "$archive" -C "$1" ./lib ./usr/include ./usr/lib
