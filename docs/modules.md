# Modules

A module is something not every installation wants. Each one declares what
it needs (a token, a binary, a port), how to **verify** that before saving
anything, and what it **installs**. Enabling a module runs the verification
first and prints one line per check: `ok`, `skip` (cannot be probed without
a side effect; it says so) or `fail`. Nothing is saved on a `fail`.

Credentials are encrypted at rest (AES-256-GCM) in `data/modules.json`; the
key is `MIDDLEWARE_SECRET_KEY` or `data/modules-key` (owner-only, generated
on first use). Disabling a module keeps its credentials; "forget
credentials" deletes them.

Binaries a module installs go under `modules/` inside the root, never into
the system, so uninstalling middleware leaves no trace outside the root and
the system package manager never fights with them. The agent CLIs are kept
current on their own: once a day the installation asks npm for the latest
release and, unless "Install newer releases automatically" is off in
Settings, installs it (`middleware deps status|check|install`, `GET
/api/v1/deps`). The container image ships them already; the daily check
replaces the shipped copy in the volume when a newer one exists.

```sh
middleware modules list
middleware modules verify cloudflare      # same checks, saves nothing
middleware modules enable cloudflare      # asks for what it needs
```

## inbound-tunnel

A reverse tunnel pushes traffic to the loopback listener. No public IP, no
certificates on this machine.

- **Needs**: nothing by itself; with the `cloudflare` module it creates the
  tunnel and its routes for you. Any other tunnel (an SSH `-R`, a tunnel from
  another provider) works if it delivers to `127.0.0.1:46100` with the
  `Host` header intact.
- **Verify**: the loopback listener answers; with cloudflare, the token can
  create and list tunnels.
- **Installs**: `cloudflared` under `modules/` when the cloudflare module is
  on. The supervisor runs it itself while the module is on and has a
  tunnel (no unit: a container has no systemd), restarts it with backoff,
  re-adopts it after a restart of its own like a site, and asks its
  `/ready` on `127.0.0.1:46101`. Its log is `data/logs/cloudflared.log`; its
  state, `GET /api/v1/modules/cloudflare/connector`.

## inbound-ports

The process listens on 80 and 443 and gets certificates itself (ACME, RFC
8555, written on the standard library). Only domains this installation
serves get certificates; the panel domain included.

- **Needs**: 80 and 443 reachable from the internet, DNS pointing here. No
  root: the unit grants `CAP_NET_BIND_SERVICE`.
- **Verify**: both ports can be bound (or are already ours); an HTTP-01
  challenge round trip against the ACME directory's staging endpoint
  (`skip` if the DNS does not point here yet, with the reason).
- **Installs**: nothing. Certificates go to `data/certs/`.

Moving domains between doors happens from the Inbound screen or with
`POST /api/v1/inbound/move`; the certificate is obtained before DNS is
touched, so a domain is never moved to a door that cannot answer it. With
the cloudflare module on, the certificate comes through dns-01 for the zones
it manages, so a brand new name gets one before its DNS points here.
The client address behind Cloudflare's proxy is believed from Cloudflare's
own ranges (built in; `data/edge/cloudflare-ips.txt` replaces them).

## cloudflare

DNS records and tunnel routes for the domains you serve.

- **Needs**: an API token. Create it at Cloudflare → My Profile → API Tokens
  → Create Token → "Create Custom Token" with exactly:
  - `Zone` · `DNS` · `Edit`
  - `Zone` · `Zone` · `Read`
  - `Account` · `Cloudflare Tunnel` · `Edit` (only for `inbound-tunnel`)
  - Zone Resources: the zones you will serve (not "All zones" unless you
    mean it).
- **Verify**: `GET /user/tokens/verify` says the token is active; each
  permission is probed by listing (zones, DNS records, tunnels). Writing is
  not probed (`skip`: "creating a record to test would leave one behind").
- **Installs**: nothing by itself.

## github

Deploy on push: a webhook calls the API, which pulls, builds and writes the
pointer ([deploy.md](deploy.md)).

- **Needs**: a webhook secret (generated for you), and for private
  repositories a way to pull: a deploy key the module generates per site
  (`site.json` → `deploy.github`), shown once so you can paste it under the
  repository's Settings → Deploy keys.
- **Webhook**: repository → Settings → Webhooks → Add webhook. Payload URL
  `https://<panel domain>/api/v1/modules/github/hook`, content type
  `application/json`, secret as shown by the module, event "Just the push
  event". The module answers 200 with what it did, or 202 "not this
  branch".
- **Verify**: the secret is set; for each site with `deploy.github`, `git
  ls-remote` works with its key (`skip` with the key to paste when it does
  not yet).
- **Installs**: nothing; `git` is a system package the installer put there.

## agent-claude

Conversations in the panel run on Anthropic's Claude through Claude Code,
the official CLI, driven as a subprocess. No SDK in the core.

**Sign in with your subscription.** The card has a "Sign in with your
subscription" button. It runs `claude setup-token` on a terminal of the
server's, with a temporary HOME so a failed attempt touches nothing, shows
you the authorization link to open on any device, and takes the code the
browser gives back. The long-lived token that comes out is stored encrypted
with the module's other credentials and handed to every turn as
`CLAUDE_CODE_OAUTH_TOKEN`; the module is enabled at the same time. The
panel shows "subscription since <date>" from then on. Signing in needs no
SSH and no browser on the server. An API key is the alternative: typed in
the card, passed as `ANTHROPIC_API_KEY`, and it wins over a stored session
(that is the CLI's rule).

- **Needs**: all optional. `model` (the alias offered first in the panel),
  `api_key`, `bin` (an executable to use instead of the managed one).
  The session (`oauth_token`, `oauth_since`) is written by the sign-in,
  not typed.
- **Verify**: the binary answers `claude --version`, or is about to be
  installed. Which credential a turn would use is said (`skip`), never
  probed: a probe would spend, and the first turn says if it is wrong.
- **Installs**: Claude Code itself, under `modules/node`, when no `bin` is
  given and none is found: `npm install -g` with a Node of our own if the
  machine has none that is new enough (fetched from nodejs.org and
  verified against the published checksum). Kept current daily from then
  on. Signing in before enabling installs it too.
- **Enable** replaces the provider the conversations run with; **disable**
  puts the CLI's own login back. Enabled modules are re-applied when the
  service starts.

Without the module the conversations still run: the `claude` CLI is looked
up in PATH and uses its own login under each site's home.

## agent-gemini

Same shape, Google's Gemini CLI. "Sign in with your Google account" runs
the CLI with `NO_BROWSER=true` on a terminal of the server's: it prints
the Google authorization link, takes the code, and writes
`~/.gemini/oauth_creds.json` in the temporary HOME. That file (it holds
the refresh token) is what gets stored encrypted, and before every turn it
is written under the run's HOME with a `settings.json` that selects that
login, which is the only way the CLI reads a session. `api_key` is passed
as `GEMINI_API_KEY` and wins over it.

## vpn

A WireGuard interface (`mw0`) so the panel and the service door are
reachable without exposing them. Peers are created in the panel; each gets
a client config shown once. "VPN only" makes the panel unreachable from
anywhere else.

- **Needs**: `endpoint` (host:port clients connect to), and optionally
  `listen_port` (51820) and `subnet` (10.77.0.0/24). Plus the helper (it
  writes `/etc/wireguard/mw0.conf` and brings the interface up) and
  `wireguard-tools`, which `middleware install` puts there with git and
  curl, and the container image ships.
- **Verify**: the values are well formed, `wg` and `wg-quick` are in PATH,
  the helper answers. Whether the endpoint reaches this machine is a
  `skip`: only a client connecting can tell.
- **Installs**: nothing; the helper's operation list has no package
  manager in it on purpose, so a missing `wireguard-tools` is reported,
  not fetched.
- The server key and the peers' preshared keys are kept in the same
  encrypted store as every other credential (`data/modules-key`), so
  `middleware vpn` and the panel read each other's state.

Inside Docker it works when the helper runs in the container: uncomment
`MIDDLEWARE_HELPER: "1"`, `cap_add: [NET_ADMIN]` and the `51820/udp` line
in `docker-compose.yml`. The entrypoint then starts the helper as root
and drops to the `middleware` user for the supervisor.

## docker

Run a site as a container instead of a process: `run.runtime: "docker"` in
its manifest. Same handover, same logs, same health check.

- **Needs**: a reachable Docker daemon the middleware user can talk to
  (member of the `docker` group, or a rootless daemon).
- **Verify**: `docker version` answers; a `hello-world` sized image can be
  run and removed (`skip` if the daemon is offline, with why).
- **Installs**: nothing; Docker is yours to install.

## Writing one

A module is a Go package implementing the module interface (key, name,
what it needs, `Verify`, `Enable`, `Disable`, routes). It may install an
external binary with `os/exec`; it may not import one. Look at the shipped
modules and at [CONTRIBUTING.md](../CONTRIBUTING.md).
