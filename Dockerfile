# Two stages: build with the Go toolchain, ship on a slim Debian with only
# what sites and modules reach for at runtime (git for deploys, curl for
# health checks and module downloads, ca-certificates for both).
#
# Inside a container there is no systemd and no privileged helper: the
# firewall, vpn and sysctl operations are not available. The supervisor,
# the proxy, ACME on 80/443 and the docker runtime (with the host socket
# mounted) all work. See docs/install.md.

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

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git curl tini \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --system middleware \
 && useradd --system --gid middleware --home-dir /srv/middleware --create-home --shell /usr/sbin/nologin middleware
COPY --from=build /out/middleware /usr/local/bin/middleware
USER middleware
WORKDIR /srv/middleware
VOLUME ["/srv/middleware"]
ENV MIDDLEWARE_ROOT=/srv/middleware
# 46100 is the loopback API and panel; 8080 and 8443 are where the ports
# door is expected to listen inside the container (map them to 80 and 443
# on the host; see docker-compose.yml).
EXPOSE 46100 8080 8443
# Any HTTP answer means the supervisor is up; the panel may answer 401 or a
# redirect to an unauthenticated probe, so -f is not used.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD curl -sS -o /dev/null http://127.0.0.1:46100/ || exit 1
# tini reaps the site processes the supervisor spawns; without an init,
# a site that forks and exits leaves zombies the kernel never collects.
ENTRYPOINT ["tini", "--", "middleware"]
CMD ["serve", "--root", "/srv/middleware"]
