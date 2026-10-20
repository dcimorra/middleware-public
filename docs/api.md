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

**Who reaches the panel.** The address lists of `/auth/panel-access` go before
everything the panel answers (403). This machine (loopback, with no proxy
header) always gets in, and a list that would shut out the caller saving it
is refused. With `passkey_only`, the password login answers 403 without
comparing anything. `middleware panel open [30m]` on the server lifts both
for a while, without a restart; the panel shows a red strip meanwhile.

**Passkeys** are WebAuthn credentials bound to the name the panel was opened
by (ES256, Ed25519, RS256; attestation not asked). They work on a secure
origin with a name, never on a bare IP, and survive a password change.

A cookie request that is not a GET must also come from the panel's own
origin (`Sec-Fetch-Site: same-origin`, or a matching `Origin`): every
subdomain of a domain is one site to the browser, so a page on a sibling
subdomain sends the panel's cookie.

**Service doors.** A token created with `scopes` opens only the routes those
scopes list, and with `:site` only for that site: `read` (the site list, one
site, its logs, the events), `restart` (a site's handover and a worker's
restart), `deploy` (a site's deploy). Anything else answers 403 and names the
scopes. For a program that acts on the house from outside, an app that builds
sites and restarts them, it is the narrowest door that works. A token
without scopes is the operator's and opens everything.

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
| GET | `/auth/me` | → `{authenticated, via: "cookie"|"token"|"socket", setup_pending, passkeys, password_closed, open_until?}` |
| GET | `/auth/tokens` | → `[{id, name, scopes?, created, last_used}]` |
| POST | `/auth/tokens` | `{name, scopes?: ["restart:shop.example", "read"]}` → `{id, token}` (shown once); 400 for a scope outside the list |
| DELETE | `/auth/tokens/{id}` | → `{ok}` |
| GET | `/auth/passkeys` | → `[{id, name, rp_id, created, last_used}]` |
| POST | `/auth/passkeys/register/options` | → `{challenge_id, publicKey}` for `navigator.credentials.create()`, bound to the name the panel is opened by |
| POST | `/auth/passkeys/register` | `{challenge_id, name, client_data_json, attestation_object}` (base64url) → the passkey; 401 when it does not verify |
| DELETE | `/auth/passkeys/{id}` | → `{ok}`; 409 for the last one while the password is closed |
| POST | `/auth/passkeys/login/options` | public → `{challenge_id, publicKey}` for `navigator.credentials.get()` |
| POST | `/auth/passkeys/login` | public, `{challenge_id, id, client_data_json, authenticator_data, signature}` → sets the cookie; 401 when refused, counted by the same brake as passwords |
| GET | `/auth/panel-access` | → `{rules: {allow, deny, passkey_only}, your_address, from_machine, open_until?}` |
| PUT | `/auth/panel-access` | `{allow, deny, passkey_only}` → rules; 400 for an unreadable entry, a list that would shut out the caller, or closing the password with no passkey |
| DELETE | `/auth/panel-open` | → `{ok}`; ends an emergency opening (`middleware panel open` starts one, from the shell only) |

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
| POST | `/sites/{domain}/exec` | `{command, timeout?: "60s"}` → `{exit_code, output, truncated?, seconds, timed_out?}`; `sh -c command` where the site runs (its folder, environment and HOME, inside its container if it has one), stdin empty, output capped at 1 MB, a minute by default and 10m at most. Every run is a `site.exec` event with the command |
| GET | `/sites/{domain}/manifest/history` | → `[{id, at, bytes}]`, newest first; the first is the manifest in force. The engine keeps every valid version it sees, whoever wrote it (50 per site, in `data/history/<domain>/`) |
| GET | `/sites/{domain}/manifest/history/{id}` | the version as it was written |
| POST | `/sites/{domain}/manifest/history/{id}/restore` | → `site`; written back through the same validation as a PUT (400 when it no longer validates), and kept as the newest version |
| POST | `/sites/{domain}/workers/{name}/restart` | → `{ok}`; stops it and the engine starts it again on its next tick, without a crash's backoff (409 when it is not running) |
| POST | `/sites/{domain}/workers/{name}/stop` | → `site`; writes `workers.<name>.disabled: true`, so it stays stopped across restarts |
| POST | `/sites/{domain}/workers/{name}/start` | → `site`; clears it |
| POST | `/sites/{domain}/stop` | → `site` (sets `disabled: true`) |
| POST | `/sites/{domain}/start` | → `site` (clears `disabled`) |
| POST | `/sites/{domain}/hold` | `{enabled, title?, message?}` → `site` |
| GET | `/sites/{domain}/logs` | `?lines=200&worker=name` → `text/plain`; `?follow=1` streams |
| GET | `/sites/{domain}/files` | `?path=/` → `[{name, dir, size, mtime}]` (inside the folder only) |
| GET | `/sites/{domain}/file` | `?path=…` → raw content |
| PUT | `/sites/{domain}/file` | `?path=…`, raw body → `{ok}` |
| POST | `/sites/{domain}/deploy` | → `{ok, build}` (runs `deploy.github.build`, writes the pointer); `{ok, via: "github"}` when the github module is enabled and the manifest has `deploy.github`: the module's runner fetches the repository first and answers at once, progress on `/events/stream` as `deploy.*` |
| GET | `/sites/{domain}/env` | → `{resolved: {…}}` what the instance was born with (secrets masked) |
| POST | `/sites/{domain}/rename` | `{to}` → site; the folder, its HOME, logs, conversations and cost move with it; published names are taken down and the new ones published on the same door |
| GET | `/sites/{domain}/download.zip` | the folder as a streamed zip without what an install or a build gives back (`node_modules`, builds, caches); `?all=1` for everything |
| GET | `/sites/{domain}/shot.png` | the site's picture, taken through a door that skips its password; 404 until there is one. Asking keeps pictures renewing for an hour |
| POST | `/sites/{domain}/shot` | take it now, whatever state the site is in; 501 without a headless Chromium |
| GET | `/archive` | archived sites, newest first → `[{name, domain, at, bytes}]` |
| POST | `/archive/{name}/restore` | → site; unpacks it back to `sites/<domain>`, 409 when that folder exists |
| GET | `/archive/{name}/download` | the archive file (`.tar.gz`) |
| DELETE | `/archive/{name}` | → `{ok}`; for good |

**The build contract.** Every command of `deploy.github.build` runs in the
site's folder with the site's resolved environment plus `MIDDLEWARE_BUILD`,
the build id (`20260131T120000Z`). A build writes into a folder named after
it and the pointer file then names that folder, which is what makes the
engine hand over without a restart: `next build` with
`distDir: process.env.MIDDLEWARE_BUILD` is the whole recipe. Without a
pointer the build runs in place and the instance is restarted. A second
deploy while one runs answers 409.

## Repositories

The git repositories inside the sites (the folder and up to three levels
below, not in dependencies, not through links). Marked ones are pulled every
two minutes: fetch, then merge the upstream, never reset or force; a merge
that collides is undone with `merge --abort` and recorded, and the first
failure of a streak is a `repo.failed` event. Pulling is not deploying.

| method | path | result |
| --- | --- | --- |
| GET | `/repos` | → `[{id, domain, path, branch, remote, upstream, head: {hash, subject, author, at}, ahead, behind, dirty, auto, last?: {at, ok, message, pulled, failures}, error?}]`; ahead and behind as of the last fetch, the remote without credentials |
| GET | `/repos/{id}/log` | → the last 30 commits |
| POST | `/repos/{id}/pull` | → the repository after a fetch and merge now |
| PUT | `/repos/{id}/auto` | `{enabled}` → the repository; marking pulls once at once. Kept in `data/repos.json` |

## Response cache

The house's cache of responses, behind the door, by the manifest's
`cache.rules` (docs/manifest.md). Responses carry `X-Middleware-Cache:
hit|miss` and, on a hit, `Age`. The budget is `cache.max_mb` in the
settings (256 by default), changed at once on save.

| method | path | result |
| --- | --- | --- |
| GET | `/cache` | → `{bytes, budget, entries}` for the whole house |
| GET | `/sites/{domain}/cache` | → `{entries, bytes, hits, misses, limit}` |
| DELETE | `/sites/{domain}/cache` | → `{ok}`; empties it (a handover does that by itself) |

## Dependency audit

`npm audit --package-lock-only` over each site with a `package-lock.json`, a
few minutes after start and then daily; never `npm audit fix`. A critical
advisory not seen in the previous audit is a `deps.critical` event (and a
push); its key carries the range, so a widened range is news again.
Publishing a site with critical advisories answers `vulnerable` beside the
lines.

| method | path | result |
| --- | --- | --- |
| GET | `/audit` | → `[{domain, at, counts: {critical, high, moderate, low, info}, critical: [{name, severity, range, title, url, key}], high: […], skipped?, error?}]` |
| POST | `/audit/run` | → `{ok}`; every site in the background, `deps.audited` when done |
| POST | `/sites/{domain}/audit` | → the site's report, now |

## Automatic repair

Off until `autofix.enabled` in the settings (with `attempts`, 2; `wait_minutes`,
30; `daily_usd`, 2). A run site with five failed starts in a row, or any site
with three failed health checks in a row, is handed to its own conversation
with the reason, its command and the last 60 log lines (fenced as evidence);
the house restarts it when the turn ends and writes the outcome to
`data/repairs.jsonl`. A manifest's `"autofix": false` keeps it away.

| method | path | result |
| --- | --- | --- |
| GET | `/autofix` | → `[{at, domain, reason, attempt, turn_id, outcome, cost_usd, recovered, skipped?}]`, oldest first |

## Push notifications

Web push (VAPID, aes128gcm) to the operator's devices. What is pushed is a
closed list: an intrusion (`guard.intrusion`), a critical (`middleware.panic`,
`deps.critical`), a repository that stopped pulling (`repo.failed`, the first
of a streak) and a turn that ended in the panel's conversations (not the site
door's). A device chooses among those kinds; a push service that says a
device is gone (404, 410) gets it forgotten.

| method | path | result |
| --- | --- | --- |
| GET | `/push` | → `{public_key, kinds, devices: [{endpoint, service, name, kinds, created, last_sent, last_error}]}` |
| POST | `/push/subscriptions` | `{endpoint, keys: {p256dh, auth}, name?, kinds?}` (the browser's PushSubscription) → `{ok}`; the same endpoint updates |
| DELETE | `/push/subscriptions` | `?endpoint=<the device's endpoint>` → `{ok}` |
| POST | `/push/test` | → `{sent, error?}` to every device, whatever it chose |

## Events

| method | path | result |
| --- | --- | --- |
| GET | `/events` | `?since=RFC3339&type=prefix&subject=domain&limit=200` → `[event]` |
| GET | `/events/stream` | `text/event-stream`; each `data:` is one event JSON |

## System

| method | path | body / result |
| --- | --- | --- |
| GET | `/docs` | → `[{name, title}]`, the documents under docs/ as embedded in this binary |
| GET | `/docs/{name}` | `text/markdown`, one of them (`api`, `manifest`, `security`…) |
| GET | `/openapi.json` | public; the OpenAPI document of every registered route |
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
| GET | `/modules` | → `[{key, name, description, enabled, installed, configured, needs: [{key, label, secret, optional, hidden, help}]}]` (a hidden need is filled by the module, not typed) |
| POST | `/modules/{key}/verify` | `{config}` → `{ok, lines: [{status: ok|skip|fail, text}]}`; nothing is written |
| POST | `/modules/{key}/enable` | `{config}` → verifies, installs, enables, saves → `{ok, lines, progress: [text]}`; a failed probe answers 200 with `ok: false` and the lines, a failed install 502 |
| POST | `/modules/{key}/disable` | → `{ok}` (keeps credentials) |
| DELETE | `/modules/{key}/credentials` | → `{ok}` (disables and forgets) |
| POST | `/modules/github/hook` | GitHub's push webhook: public, checked by its signature |
| GET | `/modules/{key}` | the module's own status when it is enabled and has one (the VPN), otherwise its entry of the list |

`config` names only the module's needs; an empty secret means "keep the one
stored", never "clear it". Modules mount their own routes under
`/api/v1/modules/{key}/…`, behind authentication, and they answer 404 while
the module is disabled. `POST /modules/github/hook` is public: GitHub
authenticates it with the HMAC signature of the webhook secret.

The modules an installation has: `cloudflare`, `github`, `vpn`,
`agent-claude` and `agent-gemini` ([modules.md](modules.md)). The agent
modules take `model`, `api_key` and `bin`, all optional; enabling one
replaces the provider the conversations run with, disabling it puts the
CLI's own login back. Their sign-in with a subscription:

| method | path | body / result |
| --- | --- | --- |
| GET | `/modules/{agent}/session` | → `{provider, session: {kind: subscription|api_key|none, since?}, binary?: {name, version, latest, outdated, managed, path}}` |
| POST | `/modules/{agent}/login` | starts the vendor's sign-in on the server → `{state, url?, error?, tail?}`; `state` is `starting`, `authorize` (open `url`, paste the code), `checking`, `done` or `failed` |
| GET | `/modules/{agent}/login` | the poll; a `done` attempt stores the session, enables the module and answers `{state: "done", session}` |
| POST | `/modules/{agent}/login/code` | `{code}` → the state; 400 when it is not the moment |
| DELETE | `/modules/{agent}/login` | cancels the attempt |
| POST | `/modules/{agent}/binary/sync` | installs or updates the CLI now → `{ok, progress}` |

## Deps

The external programs the modules run (the agent CLIs), installed under
`modules/node` and checked daily against npm.

| method | path | result |
| --- | --- | --- |
| GET | `/deps` | → `[{name, npm, path, version, latest, outdated, installed, managed, checked_at, error?}]` |
| POST | `/deps/check` | asks the registry now (and installs when auto-update is on) → `{deps, progress}` |
| POST | `/deps/{name}/install` | installs or updates one → `{ok, progress, deps}` |

## Inbound

| method | path | result |
| --- | --- | --- |
| GET | `/inbound` | → `{doors: {tunnel: {open}, ports: {open, http, https, listening, error}, local: {open, http, dns, listening, error, help}}, domains: [{domain, door, cert_expires, dns, tunnel_route, published, publish_error}], new_sites, public_ip, dns_error?}` |
| POST | `/inbound/move` | `{domains: [], to: "tunnel"|"ports"}` → `{ok, lines: [{status, text}]}` |
| PUT | `/inbound/publishing` | `{new_sites?: "tunnel"|"ports", public_ip?}` → `{new_sites, public_ip}`; an empty `public_ip` goes back to detecting it |
| POST | `/inbound/doors/{door}` | `{open}` → `doors` (door is `tunnel`, `ports` or `local`; written to `data/inbound.json` and acted on at once) |
| PUT | `/inbound/local` | `{open, http?, dns?}` → `doors`; the local door's listeners (plain HTTP and the name server), 400 when an address is malformed or collides with the ports door |
| GET | `/inbound/certificates` | → `[{domain, issued, expires, issuer}]` |
| POST | `/inbound/certificates/{domain}/renew` | → `{ok}`; 502 with the CA's reason when it fails |

`dns` (`"A 203.0.113.9"`, `"CNAME …cfargotunnel.com"`) and `tunnel_route`
(`"yes"`/`"no"`) are filled by the cloudflare module and empty when it is
off; when it is on but cannot be reached, `dns_error` says so and the rest
of the answer stands. A move is publishing names one by one, through the
same publisher as creating a site (below): to the ports door the certificate
comes first (dns-01 when the cloudflare module manages the zone, so the name
need not point here yet) and DNS only after it; the A record uses
`public_ip`, or the address the machine is seen from.

## Publishing

| method | path | result |
| --- | --- | --- |
| GET | `/publish` | → `{name: {site, door, state, error?, attempts?, next?, at?, ip?}}`; state is `published`, `pending`, `failed` (retried with backoff, 1 min doubling to 1 h), `manual` (no DNS provider, or a zone it does not manage: a person points the name) or `retiring` |
| POST | `/publish/retry` | → `{ok}`; every name not published is tried on the next pass, manual ones included |
| POST | `/sites/{domain}/publish` | `{door?}` → `{lines, vulnerable?}`; every name of the site (its domain, aliases and the implicit `www.` of an apex) on that door, kept there from now on |
| POST | `/sites/{domain}/unpublish` | → `{lines}`; tunnel rules and the records that point at this house are removed, nothing else |

`POST /sites` publishes the new site on the door for new sites when a DNS
provider is on (`"publish": false` to skip it, `"door"` to choose); the
answer carries `publishing: true` and the outcome arrives as `publish.*`
events. `DELETE /sites/{domain}` unpublishes first; a name that cannot be
removed then stays `retiring` and is retried. The intent and what the house
wrote live in `data/publish.json`, so an alias added later is published and
one removed is unpublished without anyone asking. A site folder removed by
hand keeps its records (`publish.orphan`, said once): only a deletion
through the house takes names off the internet.

## WAF

| method | path | result |
| --- | --- | --- |
| GET | `/waf` | → `{global: policy, blocked: [{ip, until, reason, strikes}], recent: [{ip, hits, status}], attack: {global: rules, counts: {domain or "panel": {served, solved}}}}` |
| PUT | `/waf` | global policy → policy (400 with the field when it does not validate) |
| POST | `/waf/block` | `{ip, duration?: "1h", reason?}` → `{ok}` (default one day; pushed to the kernel through the helper when it runs) |
| DELETE | `/waf/block/{ip}` | → `{ok}` |
| POST | `/waf/under-attack` | `{enabled, domain?, for?: "2h", bits?, life?, skip?, message?}` → `{ok}`; without `domain` the global policy, with it the site's manifest. Absent fields keep their value; `for` becomes `until` on this machine's clock and `"for": ""` is permanent (400 when the rules do not validate) |
| GET | `/waf/traffic` | → `{per_second, requests, bytes, by_domain: {domain: {requests, per_second, bytes, by_status, p50_ms, p95_ms, seconds: []}}, by_status: {…}, seconds: []}` (last minute; `seconds` oldest first) |

The routes answer 503 when the WAF did not start (a broken block list); the
`waf.error` event says what to fix.

## Security

The rules are the manifest's `security` (docs/manifest.md) and are written
through the manifest; these routes are what the panel reads beside them.

| method | path | result |
| --- | --- | --- |
| GET | `/security/catalog` | → `{families: [{key, name, note}], robots: [{name, family}], browsers: [{key, name, note}], builtin_paths: [], your_country}` |
| GET | `/sites/{domain}/security/blocks` | → `[{at, ip, reason, what, path, agent, country}]`, the last 50 refusals, newest first (kept in memory) |

## Agents

| method | path | body / result |
| --- | --- | --- |
| GET | `/agents` | → `{providers: [{provider, available, where, models: []}], capacity}` (`where` is the binary's path, or why it cannot run) |
| GET | `/agents/capacity` | → `{used, max, reserved_for_house, providers: {claude: {used, max}}, running: [{domain, provider, started, turn_id}]}`; a provider's `max` of 0 means no cap of its own |
| GET | `/sites/{domain}/chat` | → `{domain, provider, model, session_id, running, turns: [{id, role, text, cost, at, tools, questions: [{question, options, recommended}]}]}` |
| POST | `/sites/{domain}/chat` | `{text, model?}` → `{turn_id}`; progress on `/events/stream` as `agent.*`; 409 while a turn runs in this conversation; 429 when the house, or the provider, has no free slot (the message says how many of how many; `agent.capacity` is emitted with the numbers) |
| POST | `/sites/{domain}/chat/stop` | → `{ok}`; 409 when nothing runs |
| DELETE | `/sites/{domain}/chat` | → `{ok}` (new conversation) |
| POST | `/sites/{domain}/chat/reap` | → `{killed}`; stops the turn and kills everything the conversation left running (setsid escapes included) |
| GET | `/sites/{domain}/chat/processes` | → `[{pid, command, started}]` what the conversation has running now |
| POST | `/sites/{domain}/chat/processes/{pid}/kill` | → `{ok}`; 404 for a pid that is not this conversation's |
| GET | `/sites/{domain}/chat/files` | → `[{name, bytes}]` attachments that go with the next message |
| POST | `/sites/{domain}/chat/files` | `?name=file.ext`, raw body (25 MB) → `{name, bytes}` |
| DELETE | `/sites/{domain}/chat/files/{file}` | → `{ok}` |
| GET | `/sites/{domain}/chats` | the site's other conversations → `[{name, id, origin: panel|site, title, tools, created, turns, hidden, running}]`; `?all=1` includes those `claude_api.hide_chats` hides |
| POST | `/sites/{domain}/chats` | `{title?}` → `{name, id}` (201); another conversation of the panel's |
| GET | `/sites/{domain}/chats/{id}` | as `/sites/{domain}/chat` |
| POST | `/sites/{domain}/chats/{id}` | as `/sites/{domain}/chat`; 409 for a conversation the site's code opened (read and stop it, do not write into it) |
| DELETE | `/sites/{domain}/chats/{id}` | → `{ok}` |
| POST | `/sites/{domain}/chats/{id}/stop` | as above |
| POST | `/sites/{domain}/chats/{id}/reap` | as above |
| GET | `/sites/{domain}/chats/{id}/processes` | as above |
| POST | `/sites/{domain}/chats/{id}/processes/{pid}/kill` | as above |
| GET | `/sites/{domain}/chats/{id}/files` | as above |
| POST | `/sites/{domain}/chats/{id}/files` | as above |
| DELETE | `/sites/{domain}/chats/{id}/files/{file}` | as above |
| GET | `/chat/middleware` | the conversation about the installation itself, same shapes; it works in the root of the installation and may create new sites, but never edits one that already existed when its turn began |
| POST | `/chat/middleware` | as above |
| DELETE | `/chat/middleware` | as above |
| POST | `/chat/middleware/stop` | as above |
| POST | `/chat/middleware/reap` | as above |
| GET | `/chat/middleware/processes` | as above |
| POST | `/chat/middleware/processes/{pid}/kill` | as above |
| GET | `/chat/middleware/files` | as above |
| POST | `/chat/middleware/files` | as above |
| DELETE | `/chat/middleware/files/{file}` | as above |
| GET | `/agents/usage` | the Claude account's windows → `{session: {pct, reset}, week, opus?, at}` (as `/usage` shows them), cached a minute; 503 without a signed-in subscription |
| GET | `/agents/cost` | `?days=30` → `{days, books: [{book, total_usd, turns, days: {date: usd}, models: {model: usd}}]}`; a site door's spending is the book `<domain>#door` |

A message sent while a turn runs is queued (202 `{queued: true}`) and sent,
with any other queued one, when the turn ends. A turn in flight when the
supervisor restarts is picked up by the next one. A subagent's words are
kept in the turn's `threads`, apart from the answer.

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
| POST | `/tasks/{name}/stop` | → `{ok}`; ends a run in progress, recorded as `stopped`; 409 when none runs |
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
| GET | `/sites/{domain}/resources` | → `{samples: [{at, cpu, rss}], episodes: [{kind: cpu|memory, start, end?, peak}]}`; CPU in percent of one core and resident bytes, summed over every process of the site, a sample a minute for a day. An episode is five minutes or more over 90% of a core or 2 GB, and is a `resources.episode` event when it starts and `resources.episode_end` when it ends |

The rules are listed from the code that applies them: there is no way to
turn one off through the API, which is the point of a watchdog.

## The site door

The site's own code talks to Claude through the house: same engine, same
jail, same bill, visible in the panel. Open it with `claude_api` in the
manifest; the site's process then gets `MIDDLEWARE_TOKEN` (its token: it
names the site and opens only this door) and `MIDDLEWARE_API`. A docker site
gets `MIDDLEWARE_API_SOCKET` too, a unix socket mounted at its own path:
inside a container 127.0.0.1 is the container's. Through that socket only the
site door answers. Opening or closing the door restarts the site.

| method | path | result |
| --- | --- | --- |
| GET | `/site` | → `{domain, claude: {tools, model, daily_usd, spent_usd, left_usd, chats_open}}` |
| POST | `/site/claude/ask` | `{prompt, system?, model?, format?: "json", schema?, wait?}` → `{id, turn_id, text, cost_usd, json?, json_error?, error?}`; one question, one call |
| GET | `/site/claude/chats` | the site's conversations |
| POST | `/site/claude/chats` | `{title?, system?, model?, prompt?}` → `{id}` (201), or the answer when a prompt was sent |
| GET | `/site/claude/chats/{id}` | the conversation with every turn |
| POST | `/site/claude/chats/{id}/messages` | `{prompt, model?, format?, schema?, wait?}` → as ask |
| POST | `/site/claude/chats/{id}/stop` | stop the turn |
| POST | `/site/claude/chats/{id}/reap` | stop it and kill what it left running |
| DELETE | `/site/claude/chats/{id}` | delete it |

`Authorization: Bearer $MIDDLEWARE_TOKEN`. The operator may call the same
routes with a session or an API token and `X-Middleware-Site: <domain>`.
`wait` is how long the call waits for the answer (`2m` by default, `15m` at
most); `0` returns at once with `running: true` and the conversation is
polled. The manifest decides on every request: a site whose door was closed
is refused even with its token.

**Tool levels** (`claude_api.tools`): `none` (the default: it talks and
nothing else), `read` (reads the site's folder), `web` (read plus web search
and fetch; what it reads is information, not instructions), `write` (the
same jail as the panel's conversation). Levels below `write` run with an
allow list of tools (a tool the CLI adds later arrives closed), never start
subagents, and cannot read the site's own `.env`.

**Money**: `claude_api.daily_usd` (0: `agents.site_daily_usd` in settings,
5 USD by default) is checked before every turn, in UTC days. Traffic decides
this spending, not a person watching. `max_chats` (20) caps the open
conversations. The last free agent slot is never a site's: the operator can
always get in.
