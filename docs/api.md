# API v1

Everything the panel and the CLI do goes through `/api/v1`. JSON in, JSON out.
Errors are `{"error": "message"}` with a 4xx/5xx status. The API answers on
the panel domain and on the loopback listener; it is never served on a site's
domain.

## Authentication

- `Authorization: Bearer <token>` — API tokens, created in Settings.
- Session cookie `mw_session` — set by `POST /api/v1/auth/login`.
- The unix socket `data/api.sock` needs no credentials: whoever can open it
  already owns the files. The CLI uses it.

First run: `middleware setup` prints a one-time code; `POST /api/v1/auth/setup`
with `{"code": "...", "password": "..."}` creates the operator.

**CSRF.** A request authenticated by the cookie that is not a `GET`, `HEAD`
or `OPTIONS` must carry the header `X-Requested-With: middleware`, or it is
refused with 403. A page on another origin can make the browser send the
cookie but cannot add a custom header without a CORS preflight, which this
API never answers. Tokens and the socket need no header.

| method | path | body / result |
| --- | --- | --- |
| POST | `/auth/setup` | `{code, password}` → `{ok}` |
| POST | `/auth/login` | `{password}` → sets cookie, `{ok}` |
| POST | `/auth/logout` | → `{ok}` |
| GET | `/auth/me` | → `{authenticated, via: "cookie"|"token"|"socket"}` |
| GET | `/auth/tokens` | → `[{id, name, created, last_used}]` |
| POST | `/auth/tokens` | `{name}` → `{id, token}` (shown once) |
| DELETE | `/auth/tokens/{id}` | → `{ok}` |

## Sites

A site object:

```json
{
  "domain": "example.com",
  "mode": "run",
  "alive": true,
  "port": 33012,
  "revision": "9f2c…",
  "handing": false,
  "error": "",
  "managed": true,
  "manifest": { … site.json … },
  "workers": { "api": { "port": 8090, "alive": true, "error": "" } }
}
```

| method | path | body / result |
| --- | --- | --- |
| GET | `/sites` | → `[site]` (includes unmanaged folders with `managed: false`) |
| GET | `/sites/{domain}` | → `site` |
| POST | `/sites` | `{domain, manifest?, template?: "static"|"node"|"go"|"docker"}` → `site` |
| PUT | `/sites/{domain}/manifest` | full manifest → `site` (validated, written atomically) |
| PATCH | `/sites/{domain}/manifest` | partial, top-level keys merged → `site` |
| DELETE | `/sites/{domain}` | `?archive=true` moves to `data/archive/` instead of deleting |
| POST | `/sites/{domain}/restart` | → `{ok}` (forces a handover) |
| POST | `/sites/{domain}/stop` | → `site` (sets `disabled: true`) |
| POST | `/sites/{domain}/start` | → `site` (clears `disabled`) |
| POST | `/sites/{domain}/hold` | `{enabled, title?, message?}` → `site` |
| GET | `/sites/{domain}/logs` | `?lines=200&worker=name` → `text/plain`; `?follow=1` streams |
| GET | `/sites/{domain}/files` | `?path=/` → `[{name, dir, size, mtime}]` (inside the folder only) |
| GET | `/sites/{domain}/file` | `?path=…` → raw content |
| PUT | `/sites/{domain}/file` | `?path=…`, raw body → `{ok}` |
| POST | `/sites/{domain}/deploy` | → `{ok, build}` (runs `deploy.github.build`, writes the pointer); `{ok, via: "github"}` when the github module is enabled and the manifest has `deploy.github`: the module's runner fetches the repository first and answers at once, progress on `/events/stream` as `deploy.*` |
| GET | `/sites/{domain}/env` | → `{resolved: {…}}` what the instance was born with (secrets masked) |

**The build contract.** Every command of `deploy.github.build` runs in the
site's folder with the site's resolved environment plus `MIDDLEWARE_BUILD`,
the build id (`20260131T120000Z`). A build writes into a folder named after
it and the pointer file then names that folder, which is what makes the
engine hand over without a restart: `next build` with
`distDir: process.env.MIDDLEWARE_BUILD` is the whole recipe. Without a
pointer the build runs in place and the instance is restarted. A second
deploy while one runs answers 409.

## Events

| method | path | result |
| --- | --- | --- |
| GET | `/events` | `?since=RFC3339&type=prefix&subject=domain&limit=200` → `[event]` |
| GET | `/events/stream` | `text/event-stream`; each `data:` is one event JSON |

## System

| method | path | body / result |
| --- | --- | --- |
| GET | `/system` | → `{version, root, uptime, listen, panel_domain, load, memory, disk, go}` |
| GET | `/settings` | → settings |
| PUT | `/settings` | settings → settings |
| POST | `/system/restart` | → `{ok}` (SIGTERM to self; systemd restarts) |
| GET | `/system/ports` | → `{from, to, in_use: {port: domain}}` |
| GET | `/system/processes` | → `[{pid, domain, kind, rss, cpu, started}]` |

The settings object (`data/settings.json`):

```json
{
  "panel_domain": "panel.example.com",
  "listen": "127.0.0.1:46100",
  "ports": { "from": 33000, "to": 47000 },
  "pages": { "stopped": ["Title", "Message"] },
  "agents": {
    "max_concurrent": 8,
    "reserved_for_house": 1,
    "per_provider": { "claude": 0, "gemini": 0 },
    "default_provider": "claude"
  }
}
```

`agents` caps the conversations: `max_concurrent` turns at once across
every conversation (1..64), of which `reserved_for_house` (fewer than the
cap) only the `middleware` conversation may take once the rest are busy;
`per_provider` caps one provider's share, 0 meaning no cap of its own;
`default_provider` runs a site whose manifest names none. A `PUT` without
the section keeps the defaults; one that breaks the rules is a 400 naming
the field. The new limits apply to the next message, not the next restart.

## Modules

| method | path | body / result |
| --- | --- | --- |
| GET | `/modules` | → `[{key, name, description, enabled, installed, configured, needs: [{key, label, secret, optional, help}]}]` |
| POST | `/modules/{key}/verify` | `{config}` → `{ok, lines: [{status: ok|skip|fail, text}]}`; nothing is written |
| POST | `/modules/{key}/enable` | `{config}` → verifies, installs, enables, saves → `{ok, lines, progress: [text]}`; a failed probe answers 200 with `ok: false` and the lines, a failed install 502 |
| POST | `/modules/{key}/disable` | → `{ok}` (keeps credentials) |
| DELETE | `/modules/{key}/credentials` | → `{ok}` (disables and forgets) |
| GET | `/modules/{key}` | the module's own status when it is enabled and has one (the VPN), otherwise its entry of the list |

`config` names only the module's needs; an empty secret means "keep the one
stored", never "clear it". Modules mount their own routes under
`/api/v1/modules/{key}/…`, behind authentication, and they answer 404 while
the module is disabled. `POST /modules/github/hook` is public: GitHub
authenticates it with the HMAC signature of the webhook secret.

The modules an installation has: `cloudflare`, `github`, `vpn`,
`agent-claude` and `agent-gemini` ([modules.md](modules.md)). The agent
modules take `api_key`, `bin`, `model` (and `config_dir` for Claude), all
optional; enabling one replaces the provider the conversations run with,
disabling it puts the CLI's own login back.

## Inbound

| method | path | result |
| --- | --- | --- |
| GET | `/inbound` | → `{doors: {tunnel: {open}, ports: {open, http, https, listening, error}, local: {open, http, dns, listening, error, help}}, domains: [{domain, door, cert_expires, dns, tunnel_route}], dns_error?}` |
| POST | `/inbound/move` | `{domains: [], to: "tunnel"|"ports", ip?}` → `{ok, lines: [{status, text}]}` |
| POST | `/inbound/doors/{door}` | `{open}` → `doors` (door is `tunnel`, `ports` or `local`; written to `data/inbound.json` and acted on at once) |
| PUT | `/inbound/local` | `{open, http?, dns?}` → `doors`; the local door's listeners (plain HTTP and the name server), 400 when an address is malformed or collides with the ports door |
| GET | `/inbound/certificates` | → `[{domain, issued, expires, issuer}]` |
| POST | `/inbound/certificates/{domain}/renew` | → `{ok}`; 502 with the CA's reason when it fails |

`dns` (`"A 203.0.113.9"`, `"CNAME …cfargotunnel.com"`) and `tunnel_route`
(`"yes"`/`"no"`) are filled by the cloudflare module and empty when it is
off; when it is on but cannot be reached, `dns_error` says so and the rest
of the answer stands. A move to the ports door obtains the certificate first
and marks the domain only when that worked. With the cloudflare module on,
each domain that was marked is then published: a CNAME to the tunnel, or an
A record to `ip` for the ports door, with a `skip` line when `ip` was not
sent.

## WAF

| method | path | result |
| --- | --- | --- |
| GET | `/waf` | → `{global: policy, blocked: [{ip, until, reason, strikes}], recent: [{ip, hits, status}]}` |
| PUT | `/waf` | global policy → policy (400 with the field when it does not validate) |
| POST | `/waf/block` | `{ip, duration?: "1h", reason?}` → `{ok}` (default one day; pushed to the kernel through the helper when it runs) |
| DELETE | `/waf/block/{ip}` | → `{ok}` |
| POST | `/waf/under-attack` | `{enabled, domain?}` → `{ok}`; without `domain` the global policy, with it the site's manifest |
| GET | `/waf/traffic` | → `{per_second, requests, bytes, by_domain: {…}, by_status: {…}, seconds: []}` (last minute) |

The routes answer 503 when the WAF did not start (a broken block list); the
`waf.error` event says what to fix.

## Agents

| method | path | body / result |
| --- | --- | --- |
| GET | `/agents` | → `{providers: [{provider, available, where, models: []}], capacity}` (`where` is the binary's path, or why it cannot run) |
| GET | `/agents/capacity` | → `{used, max, reserved_for_house, providers: {claude: {used, max}}, running: [{domain, provider, started, turn_id}]}`; a provider's `max` of 0 means no cap of its own |
| GET | `/sites/{domain}/chat` | → `{domain, provider, model, session_id, running, turns: [{id, role, text, cost, at, tools, questions: [{question, options, recommended}]}]}` |
| POST | `/sites/{domain}/chat` | `{text, model?}` → `{turn_id}`; progress on `/events/stream` as `agent.*`; 409 while a turn runs in this conversation; 429 when the house, or the provider, has no free slot (the message says how many of how many; `agent.capacity` is emitted with the numbers) |
| POST | `/sites/{domain}/chat/stop` | → `{ok}`; 409 when nothing runs |
| DELETE | `/sites/{domain}/chat` | → `{ok}` (new conversation) |
| GET/POST/DELETE | `/chat/middleware`, `POST /chat/middleware/stop` | the conversation about the installation itself, same shapes; it works in the root of the installation and may create new sites, but never edits one that already existed when its turn began |

The `agent.*` events on the stream, all with the domain (or `middleware`) as
subject and `turn_id` in `data`: `agent.started` (`provider`, `model`),
`agent.text` (`text`, one complete piece as the agent said it), `agent.tool`
(`tool`, `input`), `agent.done` (`cost`, `session_id`, `text`, `tools`),
`agent.error` (level `error`; the turn ended badly), `agent.jail_violation`
(`paths` the agent referred to and must not have), `agent.cost_cap` (the
site's monthly cap refused the turn), `agent.capacity` (level `warn`; a
send was refused for lack of a slot: `used`, `max`, `provider`, `providers`)
and `agent.question` (`question`, `options`, `recommended`: the agent asked
for a decision).

**Questions.** An agent that needs a decision writes a fenced block in its
text:

```question
{"question": "Which database?", "options": ["SQLite", "Postgres"], "recommended": "SQLite"}
```

The block is parsed as the text arrives, emitted as `agent.question` and
kept on the turn as `questions`; the panel shows it as buttons (plus
"Something else…" for a typed answer; an empty `options` list means only
the text box) and the chosen text is sent as the next message. A block that
is not valid JSON is left in the text and ignored.

## Tasks

| method | path | body / result |
| --- | --- | --- |
| GET | `/tasks` | → `[{name, schedule, site, disabled, next, running, error, last: {at, status, duration, exit_code, trigger}}]` (a file that does not parse is a row with `error`) |
| PUT | `/tasks/{name}` | task JSON ([manifest.md](manifest.md#tasks-tasksnamejson)) → the task's row; 400 with the field when it does not validate |
| DELETE | `/tasks/{name}` | → `{ok}`; the history and the log stay |
| POST | `/tasks/{name}/run` | → `{ok, log}` at once; the run is on `/events/stream` as `task.*`, and `task.skipped` when it was already running |
| GET | `/tasks/{name}/log` | `?lines=500` → `text/plain`, the last lines of `data/logs/tasks/{name}.log` |

## VPN

| method | path | result |
| --- | --- | --- |
| GET | `/modules/vpn` | → `{enabled, up, interface, address, endpoint, listen_port, public_key, vpn_only, helper, peers: [{name, address, public_key, created, last_handshake, online, endpoint, rx, tx}], error?}` |
| POST | `/modules/vpn/peers` | `{name}` → 201 `{name, config}` (a client config, shown once, never stored) |
| DELETE | `/modules/vpn/peers/{name}` | → `{ok}` |
| POST | `/modules/vpn/only` | `{enabled}` → `{ok, enabled}`; panel and service door reachable only through the VPN. Refused while the VPN is down or has no peer |

The module is enabled like any other (`POST /modules/vpn/enable` with
`endpoint`, and optionally `listen_port` and `subnet`); its routes answer
404 until then. It needs the privileged helper: `wg` and `nft` are its
business, never this process's.

## Guard

| method | path | result |
| --- | --- | --- |
| GET | `/guard` | `?all=1` → `{rules: [{key, level, description}], incidents: [{id, at, domain, rule, level, where, action, pid, cmdline}]}`; dismissed incidents are left out unless `all=1` |
| POST | `/guard/incidents/{id}/dismiss` | → `{ok}` |

The rules are listed from the code that applies them: there is no way to
turn one off through the API, which is the point of a watchdog.
