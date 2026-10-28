# Architecture

`middleware` is one Go binary with no third-party dependencies. It runs as an
unprivileged user under systemd, listens on a loopback port (and optionally on
80/443), and is the only thing in front of every site on the machine.

```
                 ┌────────────────────────────────────────────────────────┐
  internet ──►   │  inbound: tunnel port · 80/443 (ACME)                  │
                 │     │                                                  │
                 │     ▼                                                  │
                 │  waf ──► router (Host) ──► proxy ──► site instance     │
                 │                    │                                   │
                 │                    └──► panel / api (panel domain)     │
                 │                                                        │
                 │  engine: reconcile manifests ⇄ running instances       │
                 │     runtime: process | docker                          │
                 │  modules: cloudflare · github · agents · vpn · …       │
                 │  tasks · guard · events                                │
                 └──────────────┬─────────────────────────────────────────┘
                                │ unix socket, closed op list
                 ┌──────────────▼─────────────────────────────────────────┐
                 │  middleware-helper (root): firewall · vpn · sysctl     │
                 └────────────────────────────────────────────────────────┘
```

## The rule that governs everything

**A site lives inside its folder.** `sites/<domain>/` is the whole project,
`.git` included, plus a `site.json` manifest. The manifest is the only state
that matters: stopping a site, holding it behind a page, jailing it in a
container, closing its door — all of it is a field in `site.json`, written
atomically, and all of it survives a restart because the restart re-reads it.

The root directory (`MIDDLEWARE_ROOT`, default `/srv/middleware`) holds:

```
sites/<domain>/site.json     one folder per domain
tasks/<name>.json            scheduled work
data/                        what this installation decided
  settings.json              panel domain, ports, listeners
  modules.json               which modules are on; credentials encrypted inside
  state/<domain>.json        running instance: pid, port, revision (for re-adoption)
  logs/<domain>/             site output, rotated
  events/                    audit trail, one file per day
modules/                     binaries a module installed (cloudflared, wg, agent CLIs)
```

## One process per site, always

The engine runs a reconcile loop. Every tick it loads each manifest, resolves
everything that can change underneath it (`env_from_file` pointers, the file's
own content) into a **revision hash**, and compares it with the revision of the
running instance. When they differ, it performs a **handover**:

1. start the new instance on a fresh port, output straight to its log file
   (never a pipe: a grandchild that inherits the pipe would hang `Wait`);
2. wait until the port accepts connections and the health path answers 2xx;
3. switch the router to the new port;
4. keep the old instance draining for `drain` (default 8s), then SIGTERM, then
   SIGKILL.

There is exactly one instance receiving traffic at any moment. There is no
cluster mode and there will not be one: a WebSocket or an in-memory session
needs one process, and a deployment that wants zero dropped requests gets it
from the handover, not from replicas. This is the **non-cluster contract**,
and `TestHandoverDropsNoRequests` is what keeps it honest.

### Re-adoption

Sites outlive the supervisor. The unit has `KillMode=process`, so a restart of
`middleware` leaves every site running. On boot the engine reads
`data/state/<domain>.json`, checks the pid is alive and the port is listening,
and adopts the instance without restarting it. After a binary upgrade the
operator is told that adopted instances still carry the environment they were
born with, and can roll them over with `middleware site restart --all`.

## Runtimes

`runtime.Runtime` is the interface; two implementations ship.

- **process** (default): `exec.Cmd` with a clean environment (no duplicate
  keys, `PORT` last), a per-site `HOME` under `data/homes/`, output to file,
  process group of its own so a stop reaches grandchildren.
- **docker**: the same manifest, run as a container. The image is built from
  `docker.build` or pulled from `docker.image`; the site folder is bind-mounted;
  the port is published on loopback only. Handover works the same way: two
  containers for a few seconds, then one.

Switching a site between runtimes is a manifest edit.

## Privilege

The supervisor is unprivileged. Everything that needs root goes through
`middleware-helper`, a second systemd service running the same binary with a
different subcommand, listening on `/run/middleware/helper.sock` (owner root,
group `middleware`, mode 0660). It accepts a **closed list of operations**:

| op | what it does |
| --- | --- |
| `firewall.apply` | replace the `middleware` nftables table from a validated rule set |
| `firewall.block` / `firewall.unblock` | add/remove an address from the block set, with a TTL |
| `vpn.apply` | write `/etc/wireguard/mw0.conf` from a validated config and reload |
| `vpn.up` / `vpn.down` | bring the interface up or down |
| `sysctl.reserve_ports` | reserve the site port range from the ephemeral range |
| `update.install` | replace the binary with a newer release of this repository, sha256 checked and synced to disk, keeping the old one as `middleware.prev` and a record of the update on trial next to it; then restart itself. Refuses a release it rolled back from, and anything while an update is on trial |
| `update.confirm` | the supervisor running the release on trial says it serves (listener open, half a minute up, a request answered through it); the trial ends. Answers the release skipped, if any. Without it in five minutes, or with the supervisor in a crash loop, the helper restarted on the new binary rolls back by itself: no operation asks for an older version |
| `requirements.install` | install the house's requirements (packages, docker and its group, nvm with the LTS, fail2ban, optionally a headless Chromium): this binary's own `install --requirements`, for the user and root the installed `middleware.service` names, in a transient unit of its own (`systemd-run`), its output in `/run/middleware/requirements.log`, root's (in the supervisor's `data/` a link there made root write over any file); no unit, binary or restart |

Each operation validates its input (addresses parse, keys are base64 of the
right length, names match `^[a-z0-9-]+$`), logs what it did, and never takes a
free-form string that reaches a shell. Ports 80 and 443 do not need the helper:
the unit grants `CAP_NET_BIND_SERVICE`.

## Inbound

Two doors, either or both open:

- **tunnel**: a Cloudflare tunnel (or any reverse tunnel) pushes to the
  loopback port. No public IP, no certificates here.
- **ports**: the process listens on 80 and 443 and obtains certificates by
  itself with an ACME client written against RFC 8555 on the standard library.
  Only domains this installation serves get certificates.

The panel answers through any door, always, so closing the one you came in by
cannot lock you out.

## WAF

Layer 7 lives in the core and is always on, configured per site and globally:
rate limits per route, country and user-agent policies, request body limits,
an "under attack" switch (the proof of work itself is `internal/attack`, a
hook after `security` and before `access`), and automatic blocking of
addresses that trip the limits. Blocking also goes to layer 3 through
`firewall.block` when the helper is present, so a flood never reaches Go.

## Modules

A module is something not every installation wants: it declares what it
needs, how to verify credentials, and what it installs. Enabling a module
**verifies before it saves**: every permission is probed and reported as a
line, and what cannot be probed without side effects is said so.

Shipped modules: `inbound-tunnel`, `inbound-ports`, `cloudflare` (DNS and
tunnel routes), `github` (deploy on push), `agent-claude`, `agent-agy`,
`vpn` (WireGuard for the panel and service door), `docker` (runtime).

Credentials are encrypted at rest with AES-256-GCM; the key comes from
`MIDDLEWARE_SECRET_KEY` or from `data/modules-key` (owner-only).

## Agents

`agent.Provider` is the interface a conversation runs on. An adapter turns a
prompt and a working directory into a stream of events (text, tool use, cost,
end). Every adapter drives the vendor's CLI as a subprocess so no SDK
enters the core: `claude`, `agy` (Antigravity), `openai` (Codex) and the
rest. What an agent may touch is decided
by the **jail** in the core (the site folder, nothing above, no other site),
not by the adapter.

## API and panel

Everything the panel does goes through `/api/v1`, versioned JSON, with a
bearer token or a session cookie. `/api/v1/events` is a server-sent stream of
everything that happens (instance started, handover done, certificate
renewed, address blocked). The panel is static HTML/CSS/JS embedded in the
binary, no build step; the CLI uses the same API over the unix socket.

## Linked nodes

Several installations join one network (`internal/nodes`): a WireGuard
mesh with no master, in which every node is a peer of every other on its
own interface (`mwmesh`) and address. Only traffic to another node's /32
goes through the tunnel; the machines' own traffic does not change, and
the helper's `mesh.firewall` table admits on that interface the nodes' API
and nothing else. Membership is a roster each node pulls from the others
every round, newest entry winning, removals kept as marked entries, so
joining one node is joining all of them. An invitation carries a key pair
made for the newcomer so the first contact already goes through the
tunnel; the newcomer replaces it with its own on joining. The panel keeps
per tab the node it works on and sends every call through
`/api/v1/nodes/{id}/api/…`, a streaming reverse proxy to that node's mesh
door: a build, a chat turn, an agent run on the machine that owns the
site. The dashboard is one request, `/nodes/overview`, which asks every
node's `/node/summary` in parallel.

## Events, not logs

Anything that changes state emits an event with a type, the subject and who
caused it (operator, agent, guard, schedule). Events go to the SSE stream, to
`data/events/`, and to notifiers. If something can fail and look the same as
if it worked, it emits an event either way.
