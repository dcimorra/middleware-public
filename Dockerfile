# Three stages: build the Go binary, fetch the agent CLIs with Node, ship
# on a slim Debian with what sites and modules reach for at runtime (git
# for deploys, curl for health checks, wireguard-tools for the vpn, tini
# to reap site processes, Node and the two CLIs for the conversations).
#
# The CLIs ship with the image AND update themselves: the copies here are
# the seed in /opt/middleware/deps, and the first daily check (or an
# enable) installs the current release under /srv/middleware/modules/node,
# in the volume, which then takes precedence. A container rebuilt a year
# later still has a Claude Code that works. See docs/modules.md.
#
# With cap_add NET_ADMIN and MIDDLEWARE_HELPER=1 in docker-compose.yml the
# entrypoint also runs the privileged helper, so the layer-3 firewall and
# the WireGuard vpn work inside the container; without them it runs the
# supervisor alone, as before.

FROM golang:1.24 AS build
WORKDIR /src
# No go.sum to copy: the core has no dependencies, so there is nothing to
# download and the build is fully offline.
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X main.Version=${VERSION}" \
      -o /out/middleware ./cmd/middleware

# The agent CLIs, installed with the same npm line the deps package runs
# at runtime, into a prefix the final image copies whole.
FROM node:22-bookworm-slim AS cli
ENV npm_config_update_notifier=false npm_config_fund=false npm_config_audit=false
RUN npm install -g --prefix /opt/middleware/deps @anthropic-ai/claude-code @google/gemini-cli \
 && rm -rf /root/.npm

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git curl tini wireguard-tools iproute2 nftables util-linux \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --system middleware \
 && useradd --system --gid middleware --home-dir /srv/middleware --create-home --shell /usr/sbin/nologin middleware
# Node, from the official image, so the CLIs' shebang finds it; and the
# CLIs themselves.
COPY --from=cli /usr/local/bin/node /opt/middleware/node/bin/node
COPY --from=cli /usr/local/lib/node_modules/npm /opt/middleware/node/lib/node_modules/npm
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /opt/middleware/node/bin/npm \
 && ln -s ../lib/node_modules/npm/bin/npx-cli.js /opt/middleware/node/bin/npx
COPY --from=cli /opt/middleware/deps /opt/middleware/deps
COPY --from=build /out/middleware /usr/local/bin/middleware
COPY scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint
RUN chmod 0755 /usr/local/bin/docker-entrypoint
WORKDIR /srv/middleware
VOLUME ["/srv/middleware"]
ENV MIDDLEWARE_ROOT=/srv/middleware
# The managed copies in the volume first, then the seed, then Node.
ENV PATH=/srv/middleware/modules/node/bin:/opt/middleware/deps/bin:/opt/middleware/node/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
# The container's 127.0.0.1 is not the host's: a port published by docker
# arrives on eth0 and a listener bound to loopback resets it. Bind every
# interface of the container; docker-compose.yml publishes it on the host's
# loopback only, so nothing outside the machine reaches it.
ENV MIDDLEWARE_LISTEN=0.0.0.0:46100
# Same reasoning for the local door (development bench): its defaults go
# on every interface and on unprivileged ports, which docker-compose.yml
# publishes as the host's 80 and 5353. A file that already names other
# addresses keeps them.
ENV MIDDLEWARE_LOCAL_HTTP=0.0.0.0:8080 MIDDLEWARE_LOCAL_DNS=0.0.0.0:5353
# The CLIs shipped here are the seed the updater may replace (into the
# volume); a copy elsewhere in PATH would be left alone.
ENV MIDDLEWARE_DEPS_SEED=/opt/middleware/deps/bin
# 46100 is the loopback API and panel; 8080 and 8443 are where the ports
# door is expected to listen inside the container (map them to 80 and 443
# on the host; see docker-compose.yml).
EXPOSE 46100 8080 8443
# Any HTTP answer means the supervisor is up; the panel may answer 401 or a
# redirect to an unauthenticated probe, so -f is not used.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD curl -sS -o /dev/null http://127.0.0.1:46100/ || exit 1
# The entrypoint starts as root, runs the helper when asked to, and drops
# to the middleware user for the supervisor; tini reaps the site
# processes the supervisor spawns.
ENTRYPOINT ["tini", "--", "docker-entrypoint"]
CMD ["serve", "--root", "/srv/middleware"]
