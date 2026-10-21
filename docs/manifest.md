# The manifest: `site.json`

One file per site, at `sites/<domain>/site.json`. A folder whose name has no
dot (`sites/tools/`) is a **project**: its workers, its tasks and its
conversation, and nothing that answers a hostname, so `static`, `run`,
`upstream` and `aliases` are refused there. Every field is optional except
`version`. Durations are Go durations (`8s`, `2m`). Commands are arrays,
never shell strings. Paths are relative to the site folder.

```json
{
  "version": 2,
  "title": "Shop",
  "aliases": ["shop.example.org"],

  "static": { "root": "public", "spa": false },

  "run": {
    "command": ["./server", "--port", "$PORT"],
    "cwd": ".",
    "env": { "NODE_ENV": "production" },
    "env_files": [".env", ".env.local"],
    "env_from_file": { "NEXT_DIST_DIR": ".next-current" },
    "port": 0,
    "health": { "path": "/", "timeout": "60s", "interval": "30s" },
    "drain": "8s",
    "restart": { "max_backoff": "60s" },
    "runtime": "process",
    "docker": {
      "image": "ghcr.io/acme/app:latest",
      "build": "Dockerfile",
      "volumes": ["data:/app/data"],
      "memory": "512m",
      "pids": 1024,
      "cpus": 1.5,
      "cpu_weight": 1024,
      "user": ""
    }
  },

  "upstream": "http://127.0.0.1:8090",

  "workers": {
    "api": { "command": ["./bin/api"], "cwd": "backend", "port": 8090, "env": {},
             "env_files": [".env"], "env_from_file": {}, "disabled": false, "note": "" }
  },

  "disabled": false,
  "hold": { "enabled": false, "title": "", "message": "", "mode": "maintenance", "until": "" },
  "notice": { "theme": "card" },
  "claude_api": { "tools": "none", "daily_usd": 0, "max_chats": 20, "model": "", "hide_chats": false },
  "claude": false,
  "chat": { "reads": ["other.example.com"] },
  "needs": ["api.example.com", "worker@shop.example.com"],

  "access": {
    "password": "",
    "message": "Ask Ana for the password",
    "allow": ["10.0.0.0/8"],
    "deny": ["10.9.9.9"],
    "deny_countries": ["XX"],
    "routes": {
      "/admin": { "password": "", "message": "", "allow": ["10.1.0.0/16"], "deny": [], "note": "back office" }
    }
  },

  "security": {
    "countries": { "deny": ["RU"] },
    "bots": { "deny": ["ai", "seo"], "except": ["Googlebot"], "no_ua": false,
              "window": { "from": "00:00", "to": "07:00", "zone": "Europe/Madrid" } },
    "browsers": { "allow": [] },
    "agents": { "deny": ["scrapy"] },
    "paths": ["/wp-login.php", "/xmlrpc.php"],
    "message": ""
  },

  "waf": {
    "rate": { "requests": 600, "window": "1m", "burst": 100 },
    "max_body": "20m"
  },

  "attack": { "on": false, "bits": 16, "life": "30m", "until": "", "skip": ["/hooks/stripe"], "message": "" },

  "cache": {
    "static_max_age": "1h",
    "rules": [
      { "path": "/product", "mode": "origin" },
      { "path": "/img", "mode": "force", "ttl": 3600, "note": "logos, never change" },
      { "path": "/product/admin", "mode": "never" }
    ],
    "max_mb": 64
  },
  "headers": { "X-Frame-Options": "DENY" },

  "deploy": {
    "github": {
      "repository": "acme/app",
      "branch": "main",
      "build": [["npm", "ci"], ["npm", "run", "build"]],
      "pointer": ".next-current"
    }
  },

  "agent": { "provider": "claude", "model": "", "monthly_cap_usd": 0 }
}
```

## What answers a request

Exactly one of these decides, checked in this order:

1. `disabled: true` → the "stopped" page. No process runs.
2. `hold.enabled: true` → the holding page. No process runs.
3. `run` → the supervised instance. Started, health-checked, handed over.
4. `upstream` → a process the middleware does not supervise (a fixed port
   someone else owns). Proxied as is.
5. `static` → files from `static.root` (default `public`, then the folder
   itself). `spa: true` serves `index.html` for unknown paths.

A folder with no `site.json` is **not served** and the panel lists it as
"unmanaged", because a folder that serves silently and a folder that is
ignored silently look the same from outside.

## Variables

In `run.command`, `run.env` values, `upstream` and `workers.*.command`:

| variable | value |
| --- | --- |
| `$PORT` | the port the instance must listen on (assigned by the engine) |
| `$DOMAIN` | the site's domain |
| `$SITE_DIR` | absolute path of the site folder |

The process also receives `PORT`, `HOST=127.0.0.1`, `MIDDLEWARE_DOMAIN`,
`MIDDLEWARE_SITE_DIR` and `HOME` (a per-site home under `data/homes/`).

## Visitor pages, hold and needs

When the site cannot answer, the visitor gets a page in the site's design
(`notice.theme`: sober, card, console, note, poster, blueprint, glitch; empty
is the house's, set in settings as `visitor_theme`). The page names the site
(`title`, else the domain), says the state, and tells **starting** (reloads
itself in five seconds) from **not answering** (a crash or a failing health
check). An API client that asked for JSON gets JSON. Upstream errors are 503,
never 502: Cloudflare replaces an origin's 502 with its own page.

`hold.mode` is what a held site answers: `maintenance` (503, the default),
`404`, `410` (gone; search engines drop it) or `500`. `hold.until` (RFC 3339)
is shown and sent as `Retry-After`.

`needs` names what the site needs up: another site, or `worker@site`. A first
start waits up to a minute for them, then starts anyway and says so; the
panel names whatever is missing now.

## The site door: `claude_api` and `claude`

`claude_api` lets the site's own code ask Claude through the house (see
api.md, "The site door"): tool level, daily cap, conversations kept, model.
Raising `tools` above `none` is a decision that matters: the site's process
answers the internet, and what an injected prompt can reach depends on it.

`claude: true` puts the house's Claude subscription token in the site's
environment (`CLAUDE_CODE_OAUTH_TOKEN`) for a site that runs the CLI itself.
It reaches as far as the subscription does; prefer `claude_api`.

## Loans: `chat.reads`

Other sites of this house whose folder this site's conversation may read,
never write. The folder is lent whole, its `.env` included: closing the
secrets and leaving the rest would be a wall with a hole in it, which is why
each one is written by hand.

## Docker

The folder and the site's HOME are mounted at their own paths, so nothing in
the command changes between runtimes. Every container has no capabilities,
`no-new-privileges`, no restart policy (the engine restarts), at most `pids`
processes (default 1024), the port on loopback only, and `HOST=0.0.0.0`
inside. `user` empty is right for the daemon: the supervisor's uid on a
rootful daemon, none on a rootless one (where root inside is the supervisor
outside). Containers carry the label `middleware.house`; leftovers no site
claims are removed when the engine starts. `middleware exec <domain> -- …`
runs a one-off command inside the site's image with the same mounts.

## Paths stay inside the site

`static.root`, `cwd`, `env_files`, `env_from_file`, `docker.build` and the host
side of `docker.volumes` must resolve inside the site folder, links included;
a manifest that names anything outside is refused when it is read. Docker
volumes are either a path in the site (`"./uploads:/u"`) or a name
(`"data:/app/data"`); a name is per site (`mw-<domain>-data`), so two sites
writing `data` do not share one.

## `env_from_file`: deploying by pointer

Each entry sets a variable to the **contents of a file**, trimmed. The file is
re-read on every tick, and a change in its contents changes the instance's
revision, which triggers a handover. A deployment is then:

```
build into .next-2026-10-19T10-00/   (while the current build keeps serving)
echo .next-2026-10-19T10-00 > .next-current
```

Rolling back is writing the previous name.

## Revision

The revision is a hash of everything the instance was born from: the resolved
command, the resolved environment (including `env_from_file` contents and
`env_files`), the runtime and docker settings. Anything else in the manifest
(`access`, `waf`, `headers`, `hold`) is applied live and does not restart the
process.

## Fields worth a line

- `env_files` are read in order, a later file overriding an earlier one, and
  `env` over all of them. `KEY=VALUE` lines, or a `.json` object of strings.
  A missing file is not an error.
- `run.port` is written only when something outside the house already knows
  it. Such a site cannot hand over (one port holds one generation), so a
  change stops the old process before starting the new one.
- `health.timeout` is how long a new generation has to get ready.
  `health.interval` is how often a running one is asked afterwards (default
  30s, stretched up to four times while it keeps answering, back to the
  interval on the first failure). Static sites are checked for their
  `index.html`. The panel shows the last checks; every change is an event.
- `access`: every rule narrows the one above it, never widens it. A zone's
  `allow` narrows the site's, `deny` adds to it, and a zone's own `password`
  is asked for after the site's. Each password gives a pass valid for its own
  zone only. Rules decide on the cleaned, case-folded path, which is what the
  site will serve. Every `allow` and `deny` entry is an address or a CIDR;
  one that is neither rejects the whole manifest rather than being skipped,
  because a skipped entry can leave an allow list empty, and an empty one
  admits everyone. Passwords are stored hashed; set them from the panel or
  the API, which hash what they are given.
- `security` decides by what a caller claims to be, and is the softest door
  on purpose: the country is the edge's and can be believed, the
  User-Agent is written by the caller and can say anything. It clears out
  what identifies itself (crawlers, AI crawlers, scripts), not anyone who
  insists; `access` is for that. Four lists, all of which must pass:
  `agents` (User-Agent fragments, two characters at least), `bots`
  (families: `search`, `ai`, `social`, `seo`, `tools`, `monitors`,
  `other` for anything that says bot, crawler or spider and is in no
  table; `except` by robot name; `no_ua` closes requests without a
  User-Agent; `window` lets the closed families in between two times in
  `zone`, the machine's if empty), `browsers` (`edge`, `opera`, `samsung`,
  `ie`, `firefox`, `chrome`, `safari`, `other`, `none`) and `countries`
  (two letters, `XX` unknown, `T1` Tor). In each, deny first, and an allow
  list with anything in it means only those. An allow list of countries
  shuts out a request that arrives without one. A refused visitor gets 403
  with a line that says why but not what the rule holds (`message`
  replaces it); a robot refused by family with a window is told the hours.
  Before all of that, every site answers 404 to what no website serves:
  `.git/`, `.ssh/`, `.env*`, private keys, `/etc/passwd` and the like
  (`GET /api/v1/security/catalog` lists them), plus the site's own
  `paths`, matched by whole segments. `/.well-known` is never cut and
  cannot be listed. An unknown family, browser, country or a malformed
  time rejects the manifest. The last 50 refusals per site are kept in
  memory for the panel.
- `attack` is under-attack mode: before anything is served, the browser
  finds a number whose SHA-256 with the challenge starts with `bits` zero
  bits (8 to 24, default 16; each bit doubles the work, checking it is one
  hash) and gets a pass for `life` (default 30m), signed with a key kept on
  disk, so a restart sends nobody back, and bound to the address and
  User-Agent that solved it. It guards nothing (whoever runs JavaScript gets
  through): `access` does that. While it is on, what is not a browser does
  not get in; `skip` lists the paths that never get the challenge, by whole
  segments, and each line is an open door. A POST interrupted by it is lost.
  `until` (RFC 3339) switches it off by itself; empty is permanent. The
  clock stops charging at once and the house writes it down as off within a
  minute, keeping `bits` and `skip`. It runs after `security` and before the
  password, so a flood never reaches a key derivation without paying first.
  The WAF policy's `attack` applies to every site whose own is off (with the
  site's `skip`) and to the panel, except requests that carry their own
  credentials (an API token, the site door). The old `waf.under_attack:
  true` is read as `attack.on` with the defaults.
- `cache.rules` are the house's response cache, kept in memory and served
  without asking the site, behind the door so nothing passes a password.
  `origin` keeps what the site marks (`s-maxage`, else `max-age`) for as
  long as it says, so switching it on over an app that sends nothing changes
  nothing; `force` keeps `ttl` seconds whatever the site says, for what is
  known to be public and still; `never` keeps nothing, for `/admin`.
  Prefixes go by whole segments and the longest wins. What no rule can
  change: nothing with `Set-Cookie`, `private`, `no-store` or `no-cache`,
  `Vary: *`, `Cookie` or `Authorization`, nothing but a 200 to GET or HEAD,
  nothing for a request with `Authorization`, no event streams. Simultaneous
  misses reach the site as one, and a handover empties the site's cache.
  `max_mb` asks for less than the house's budget, never for more.
- `vpn` makes the site exist only inside the VPN (the module of that name):
  it is served through the VPN door (the server's address in the tunnel, on
  80 and 443, open while the VPN is up) and through no other, where it gets
  the same page as a name nobody serves, whatever the headers say. It is
  never published, and a published site that becomes VPN-only is taken
  down. `vpn.devices` narrows it to those peers, by their address in the
  tunnel, which WireGuard makes trustworthy. The VPN's own resolver, which
  every device's config points at, answers the house's names with the
  server's address in the tunnel and no AAAA, and forwards every other
  name. Its certificate needs dns-01 (the Cloudflare module): no CA can
  reach it.
  `vpn.paths` keeps only those paths inside the VPN (`["/admin"]` covers
  `/admin` and everything under it, not `/administrator`; the path is
  cleaned first, so `//admin` is `/admin`): the rest of the site stays
  public and published, and those paths get, outside the VPN, the page of a
  name nobody serves. A device reaches them because the VPN's resolver
  sends it to the VPN door for every name of the house. The panel sets all
  of this from the site's Access tab. The panel itself can be kept inside
  the VPN from Inbound → VPN; the machine's own loopback (SSH, the CLI)
  still reaches it, which is the way back.
- `autofix: false` keeps the automatic repair (Settings) away from this
  site, whatever the installation says; the panel cannot override it.
- `aliases` are served exactly like the site. A name that is already a site,
  or that two sites claim, is served by neither, and an event says so.

## Workers

A worker is a process that must always be up and answers no domain: a backend
behind a frontend, an internal API, a queue consumer. It is declared inside
the site (or project) it belongs to. With no `port`, one is assigned and
passed in `$PORT`; write it only when whoever calls the worker already knows
it. Workers are supervised like instances, saved to `data/state` and
re-adopted after a supervisor restart, but never handed over: they restart.
`disabled` stops one without deleting it.

## Tasks: `tasks/<name>.json`

```json
{
  "version": 2,
  "schedule": "0 3 * * *",
  "site": "example.com",
  "command": ["./scripts/backup.sh"],
  "timeout": "1h",
  "overlap": "skip",
  "env": { "KEEP_DAYS": "30" },
  "disabled": false
}
```

`site` makes the command run inside that site's folder with that site's
environment. Without it, the task runs from the root directory. `overlap` is
`skip` (default) or `queue`.
