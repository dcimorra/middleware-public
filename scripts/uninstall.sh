#!/bin/sh
# Stops middleware and removes its units. The root directory and every site
# in it are kept; say so before anyone types this from memory.
set -eu

die() { printf 'uninstall.sh: %s\n' "$*" >&2; exit 1; }

if [ "$(id -u)" = "0" ]; then
  die "do not run as root. Run as the user that installed middleware; sudo is used where needed."
fi

BIN="$(command -v middleware || true)"
[ -n "$BIN" ] || BIN=/usr/local/bin/middleware
[ -x "$BIN" ] || die "middleware is not installed ($BIN not found)"

exec "$BIN" uninstall "$@"
