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
| POST | `/sites/{domain}/deploy` | → `{ok}` (runs `deploy.github.build`, writes the pointer) |
| GET | `/sites/{domain}/env` | → `{resolved: {…}}` what the instance was born with (secrets masked) |

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

## Modules

| method | path | body / result |
| --- | --- | --- |
| GET | `/modules` | → `[{key, name, description, enabled, needs: [{key, label, secret}], installed}]` |
| POST | `/modules/{key}/verify` | `{config}` → `{ok, lines: [{ok|skip|fail, text}]}` |
| POST | `/modules/{key}/enable` | `{config}` → verifies, installs, saves → `{ok, lines}` |
| POST | `/modules/{key}/disable` | → `{ok}` (keeps credentials) |
| DELETE | `/modules/{key}/credentials` | → `{ok}` |
| GET | `/modules/{key}` | module-specific state (see each module's doc) |

Modules mount their own routes under `/api/v1/modules/{key}/…`.

## Inbound

| method | path | result |
| --- | --- | --- |
| GET | `/inbound` | → `{doors: {tunnel: {open}, ports: {open, http, https}}, domains: [{domain, door, dns, cert_expires, tunnel_route}]}` |
| POST | `/inbound/move` | `{domains: [], to: "tunnel"|"ports"}` → `{ok, lines}` |
| GET | `/inbound/certificates` | → `[{domain, issued, expires, issuer}]` |
| POST | `/inbound/certificates/{domain}/renew` | → `{ok}` |

## WAF

| method | path | result |
| --- | --- | --- |
| GET | `/waf` | → `{global: policy, blocked: [{ip, until, reason}], recent: [{ip, hits, status}]}` |
| PUT | `/waf` | global policy → policy |
| POST | `/waf/block` | `{ip, duration, reason}` → `{ok}` |
| DELETE | `/waf/block/{ip}` | → `{ok}` |
| POST | `/waf/under-attack` | `{enabled, domain?}` → `{ok}` |
| GET | `/waf/traffic` | → `{per_second, by_domain: {…}, by_status: {…}}` (last minute) |

## Agents

| method | path | body / result |
| --- | --- | --- |
| GET | `/agents` | → `[{provider, available, models: []}]` |
| GET | `/sites/{domain}/chat` | → `{provider, model, turns: [{role, text, cost, at}]}` |
| POST | `/sites/{domain}/chat` | `{text, model?}` → `{turn_id}`; progress on `/events/stream` as `agent.*` |
| POST | `/sites/{domain}/chat/stop` | → `{ok}` |
| DELETE | `/sites/{domain}/chat` | → `{ok}` (new conversation) |
| GET | `/chat/middleware` | the conversation about the installation itself (same shape) |

## Tasks

| method | path | body / result |
| --- | --- | --- |
| GET | `/tasks` | → `[{name, schedule, site, next, last: {at, status, duration}}]` |
| PUT | `/tasks/{name}` | task JSON → task |
| DELETE | `/tasks/{name}` | → `{ok}` |
| POST | `/tasks/{name}/run` | → `{ok}` |
| GET | `/tasks/{name}/log` | → `text/plain` |

## VPN

| method | path | result |
| --- | --- | --- |
| GET | `/modules/vpn` | → `{interface, address, listen_port, public_key, peers: [{name, address, last_handshake}]}` |
| POST | `/modules/vpn/peers` | `{name}` → `{config}` (a client config, shown once) |
| DELETE | `/modules/vpn/peers/{name}` | → `{ok}` |
| POST | `/modules/vpn/only` | `{enabled}` → panel and service door reachable only through the VPN |

## Guard

| method | path | result |
| --- | --- | --- |
| GET | `/guard` | → `{rules: [{key, level, description}], incidents: [{at, domain, rule, level, where, action}]}` |
| POST | `/guard/incidents/{id}/dismiss` | → `{ok}` |
