#!/bin/sh
# Builds middleware from this checkout and, unless --no-install is given,
# runs `middleware install` with the result. Needs Go (the version in go.mod
# or newer); nothing else, because the core has no dependencies to fetch.
#
#   scripts/build-from-source.sh                 build and install
#   scripts/build-from-source.sh --no-install    build only, into ./middleware
#   scripts/build-from-source.sh -- --root /opt/middleware --no-helper
set -eu

die() { printf 'build-from-source.sh: %s\n' "$*" >&2; exit 1; }

command -v go >/dev/null 2>&1 || die "go is required: https://go.dev/dl/"

HERE="$(cd "$(dirname "$0")/.." && pwd)"
cd "$HERE"

INSTALL=1
while [ $# -gt 0 ]; do
  case "$1" in
    --no-install) INSTALL=0; shift ;;
    --) shift; break ;;
    *) break ;;
  esac
done

# The version is the nearest tag, or the commit when there is none, so
# `middleware version` on a server says which source it came from.
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"

printf 'Building middleware %s\n' "$VERSION" >&2
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.Version=$VERSION" -o middleware ./cmd/middleware
printf 'Built ./middleware\n' >&2

if [ "$INSTALL" = "1" ]; then
  if [ "$(id -u)" = "0" ]; then
    die "built, but not installing as root: run ./middleware install as the operator user"
  fi
  exec ./middleware install "$@"
fi
