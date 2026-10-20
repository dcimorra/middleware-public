#!/bin/sh
# Entrypoint of the image. It exists for one reason: the privileged helper
# must run as root and the supervisor must not, and a container has one
# user. So the container starts as root, runs the helper in the background
# when MIDDLEWARE_HELPER=1 (which only works with cap_add NET_ADMIN, see
# docker-compose.yml), and hands the supervisor to the middleware user
# with setpriv. Started as another user (compose `user:`), it runs the
# command as is: that is the bind-mount case, where there is no helper.
set -eu

ROOT="${MIDDLEWARE_ROOT:-/srv/middleware}"
RUN_DIR=/run/middleware

if [ "$(id -u)" != "0" ]; then
  exec middleware "$@"
fi

# The volume is created by docker owned by root; the supervisor writes
# into it as middleware. A root-owned volume on first start is the
# "permission denied" that looks like a bug in setup.
chown middleware:middleware "$ROOT" 2>/dev/null || true
mkdir -p "$RUN_DIR"
chown middleware:middleware "$RUN_DIR"

if [ "${MIDDLEWARE_HELPER:-0}" = "1" ]; then
  middleware helper --socket "$RUN_DIR/helper.sock" --allow-gid middleware &
  # A moment for the socket, so the supervisor's first look finds it.
  i=0
  while [ ! -S "$RUN_DIR/helper.sock" ] && [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done
fi

exec setpriv --reuid=middleware --regid=middleware --init-groups \
  env HOME="$ROOT" middleware "$@"
