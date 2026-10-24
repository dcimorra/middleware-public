# middleware

**Build, test and ship from one place.**

For many web projects the usual stack (a repo, CI, a server, a proxy, DNS,
certificates, a dashboard per service and a laptop with everything
installed) is too much machinery. middleware replaces it in those cases
with one binary on your own server:

- **Build** by talking to AI agents (Claude, Codex, Antigravity…) that work
  inside each site's folder and cannot leave it.
- **Test** on the server where it will live, with the same data and the
  same environment.
- **Ship** with one button: DNS, certificate and a deploy that drops no
  request.
- **Without trading away security:** each site with its own HOME and
  environment, each agent jailed and audited, the panel behind a VPN if
  you want.

**Outgrown one machine? Add another in under 2 minutes.** Make a code in the
panel, paste it on the other machine, and they form an encrypted network.
One panel sees and manages them all, and a site moves from one to another
with a stop of seconds, its DNS following on its own.

## How it works

One Go binary in front of every site on a machine. It reads the `Host`
header and either serves `sites/<domain>/` or forwards to the process that
site's manifest describes. One process per site, always; deployments hand
over by pointer with zero dropped requests; sites outlive the supervisor.
Open source, MIT, no third-party dependencies.

```sh
curl -fsSL https://raw.githubusercontent.com/dcimorra/middleware-public/main/scripts/install.sh | sh
```

```
sites/
  example.com/        ← the whole project, .git included
    site.json         ← the only state the site has
    public/
  api.example.com/
    site.json         → { "run": { "command": ["./server", "--port", "$PORT"] } }
```

Create the folder, write the manifest, and it is served. No restart, no
registration, no port to pick.

## What it does

- **Serves and supervises.** Static folders, supervised processes, Docker
  containers and externally managed upstreams, all from the same manifest.
  Health checks, crash restarts with backoff, per-site logs, per-site `HOME`.
- **Hands over without a cut.** `env_from_file` turns a deployment into
  "write a file": the new build starts on a fresh port, gets health-checked,
  takes the traffic, and the old one drains. One process receives traffic at
  any moment. The test that enforces it runs two deployments under load and
  fails on a single dropped request.
- **Survives its own restart.** Sites keep running while the supervisor
  restarts or upgrades, and are re-adopted when it comes back.
- **Opens three doors.** A reverse tunnel pushing to the loopback port, or
  direct 80/443 with certificates obtained by an ACME client written on the
  standard library. Both at once while you move domains from one to the other.
  And a local door for development: plain HTTP plus a name server of its
  own, so `misite.middleware` opens in your browser with no internet.
- **Filters.** A layer-7 WAF (rate limits, auto-blocking, body limits,
  country and user-agent policies, an "under attack" mode) and per-site
  access control (allow lists, passwords, per-route rules). Blocks go to the
  kernel through the privileged helper when it is installed.
- **Watches.** A guard that reads `/proc`, attributes processes to sites,
  and acts on written rules: a site reaching into another site's folder is
  killed and switched off, and the incident is on record.
- **Deploys from GitHub.** A signed webhook fetches, builds and writes the
  pointer. Rollback is writing the previous build's name.
- **Talks to agents.** One conversation per site with Claude, Codex,
  Antigravity and more, driven through their CLIs, jailed to the site's
  folder, with a cost ledger, a monthly cap and the subscription's usage
  windows under the message box. Sign in with your Claude subscription or your Google
  account from the panel; the CLIs install themselves and stay current.
- **Reaches the panel privately.** A WireGuard VPN module, peers created
  from the panel, and a switch that makes the panel reachable only through
  it.
- **Schedules.** Cron tasks that run inside a site's environment, with
  overlap control, timeouts and a run history.
- **Shows everything.** A panel with no build step, a versioned JSON API,
  a server-sent event stream of everything that happens, and a CLI that uses
  the same API over a unix socket.

## How it is built

The supervisor runs as an unprivileged user. Everything that needs root goes
through `middleware-helper`, a second service with a closed list of
operations (firewall rules, WireGuard config, port reservation), each with
typed, validated input. Credentials are encrypted at rest. The core imports
nothing outside Go's standard library, and CI fails if that changes;
anything that needs an external program (cloudflared, wireguard-tools, an
agent CLI) is a module that installs it when enabled and keeps it current.

Read [`docs/architecture.md`](docs/architecture.md) for the design and the
reasoning behind it.

## Documentation

| | |
| --- | --- |
| [`docs/install.md`](docs/install.md) | One-liner, from source, Docker, what gets installed where |
| [`docs/manifest.md`](docs/manifest.md) | Every field of `site.json` and `tasks/<name>.json` |
| [`docs/deploy.md`](docs/deploy.md) | Deploying by pointer, with GitHub, rollback, the zero-downtime guarantee |
| [`docs/modules.md`](docs/modules.md) | Cloudflare, GitHub, Claude, Antigravity, VPN, the inbound doors |
| [`docs/cli.md`](docs/cli.md) | Every subcommand |
| [`docs/api.md`](docs/api.md) | The JSON API the panel and the CLI use |
| [`docs/security.md`](docs/security.md) | Threat model: what is protected and what is not |
| [`docs/upgrade.md`](docs/upgrade.md) | Swapping the binary without touching the sites |
| [`docs/architecture.md`](docs/architecture.md) | How the pieces fit |

## Quick tour

```sh
middleware install                       # units, helper, sysctl; prints a one-time code
middleware setup                         # set the operator password
middleware site add example.com --template static
echo '{ "version": 2, "run": { "command": ["./server", "--port", "$PORT"] } }' \
  | middleware site add api.example.com --manifest -
middleware site list
middleware site logs api.example.com -f
middleware modules enable cloudflare token=@/path/to/token
middleware inbound open ports
middleware events -f
```

The panel answers at `http://127.0.0.1:46100` and on the panel domain you
set in Settings.

## Status

This is version 2, a rewrite of a middleware that has run a few dozen
production domains since 2026. The core (manifest, engine, runtimes, proxy,
doors, ACME, WAF, guard, modules, agents, tasks, API, CLI, panel, installer)
is complete and tested; the test suite runs real processes, real listeners
and real TLS handshakes. Tests that need an external service (Pebble for
ACME, a Cloudflare token, root for nftables) are gated by environment flags
documented in each test file.

Linux only, by design. systemd for the service units; Docker optional.

## Contributing

[`CONTRIBUTING.md`](CONTRIBUTING.md) has the three rules that do not move:
no dependencies in the core, everything in English, a comment says why.
Security issues go through [`SECURITY.md`](SECURITY.md), not a public issue.

## License

MIT. See [`LICENSE`](LICENSE).
