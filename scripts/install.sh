#!/bin/sh
# Bootstrap for: curl -fsSL https://raw.githubusercontent.com/dcimorra/middleware-public/main/scripts/install.sh | sh
#
# Downloads the latest release binary for this machine, verifies its sha256
# against the file published next to it, and hands over to `middleware
# install`, which does the actual work and explains every step. POSIX sh on
# purpose: the machine may have nothing but busybox.
set -eu

REPO="dcimorra/middleware-public"
BASE="https://github.com/$REPO/releases/latest/download"

say() { printf '%s\n' "$*" >&2; }
die() { say "install.sh: $*"; exit 1; }

# Everything that needs root goes through sudo from `middleware install`.
# Running the whole thing as root would make root the owner of the sites.
if [ "$(id -u)" = "0" ]; then
  die "do not run as root. Run as the user that will operate the panel; sudo is used where needed."
fi

command -v curl >/dev/null 2>&1 || die "curl is required to download the release"
command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 || die "sha256sum or shasum is required to verify the download"

case "$(uname -s)" in
  Linux) ;;
  *) die "middleware runs on Linux (systemd); for other systems see docs/install.md (Docker)" ;;
esac

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "no release binary for $(uname -m); build from source with scripts/build-from-source.sh" ;;
esac

NAME="middleware-linux-$ARCH"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

say "Downloading $BASE/$NAME"
curl -fsSL -o "$TMP/$NAME" "$BASE/$NAME"
curl -fsSL -o "$TMP/$NAME.sha256" "$BASE/$NAME.sha256"

# The .sha256 file is "<hash>  <name>", as sha256sum writes it.
WANT="$(cut -d' ' -f1 "$TMP/$NAME.sha256")"
if command -v sha256sum >/dev/null 2>&1; then
  GOT="$(sha256sum "$TMP/$NAME" | cut -d' ' -f1)"
else
  GOT="$(shasum -a 256 "$TMP/$NAME" | cut -d' ' -f1)"
fi
[ -n "$WANT" ] || die "empty checksum file"
[ "$WANT" = "$GOT" ] || die "checksum mismatch: expected $WANT, got $GOT. Not installing."
say "Checksum verified."

chmod 0755 "$TMP/$NAME"

# From here on the binary explains itself: it copies itself to
# /usr/local/bin and prints one line per step. Extra arguments to this
# script (for example --root /opt/middleware --no-helper) are passed through.
# Not exec'd, so the trap above removes the download afterwards.
if [ -t 0 ]; then
  "$TMP/$NAME" install "$@"
elif [ -c /dev/tty ]; then
  # Piped through sh, stdin is the script. sudo still needs a terminal for
  # its password prompt, so reattach it; the confirmation is skipped.
  "$TMP/$NAME" install --yes "$@" </dev/tty
else
  "$TMP/$NAME" install --yes "$@"
fi
