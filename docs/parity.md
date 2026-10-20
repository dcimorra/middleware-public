# Parity with V1

V2 replaces V1 only when it does everything V1 does. This is the list of
what V1 has and V2 does not yet, by area, in the order it will be done. It
is not a porting list: each feature is rebuilt the V2 way (its packages,
its hooks, its manifest), and V1 is read for *what* it does and *what it
learned*, never for code to copy.

A line is removed from this file in the commit that makes it true, with the
test that proves it.

## Done

- Manifest paths cannot leave the site folder, symlinks included
  (`manifest.CheckInside`); docker volumes are checked and named volumes are
  per site.
- Static files are not served through a link out of the site.
- One rule for trusting the edge (`internal/edge`): the WAF, the login brake,
  the access door and the proxy all ask it. Forged edge headers no longer
  reach sites or dodge the panel's brake; `CF-IPCountry` is wired, so country
  rules work.
- Route rules decide on the cleaned, case-folded path.
- Access fails closed: without its key, protected sites answer 503.
- The engine no longer panics when a running site loses its `run` section.
- Adoption checks the pid's start time (a reused pid is not adopted).

## 1. Migration

Done: `middleware migrate` (report, `--apply`, `site.v1.json` backup), and the
manifest gained what V1 sites use: `title`, `aliases`, `env_files`, a written
`run.port`, `health.interval` with continuous checks, workers with an
assigned port, `env_from_file`, `disabled`, `note` (saved and re-adopted),
zones with their own password and deny lists, `access.message` and `deny`,
projects without a domain. What still blocks a V1 site is listed by
`middleware migrate` itself, with the section here that brings it.

## 2. Getting a site online

Done: one publisher for creation, deletion and moves (`internal/publish`),
every name of a site (aliases and the implicit `www.` included, `www: false`
to drop it) published and certified; the door for new sites and the public
address (detected, or written); dns-01 through Cloudflare; Cloudflare's
ranges as trusted edge hops; `cloudflared` run, re-adopted and watched by the
supervisor; moves serialized; panel and CLI. Proven live on 20 October
2026 against the real API, with a throwaway tunnel and name: published on
creation and answered from the internet through the connector the
supervisor ran (cloudflared 2026.10.2), the connector re-adopted across a
supervisor restart with the site up, a dns-01 certificate from Let's
Encrypt staging with its TXT removed, the visitor's real address in the
access log, and the route and record gone on deletion.

Left:
- House response cache (`cache` modes in the manifest), behind the door.
- VPN-only sites and the VPN's own DNS.
- Cloudflare extras: edge purge and cache rules, DNS record editor, Access
  app list, narrowing a wide token. Domain search and prices.

## 3. The door

- Brake on site password checks (per domain, zone and address) and at most
  two key derivations at once.
- An unreadable CIDR rejects the manifest.
- The `security` block: bot families, browser lists, country allow lists,
  per-site paths, built-in probe paths (`/.env`, `/.git`, ...), recent blocks.
- Attack mode with proof of work, a key that survives restarts, `skip` paths,
  `until`, counters, and panel exemptions.
- Panel: passkeys (docs claim them), passkey-only, panel IP allow list,
  emergency open/close from the shell, CSRF across sibling subdomains.
- A visitor page on panic (the rest of the visitor pages is done: seven
  designs, starting vs not answering, hold modes and `until`, JSON for API
  clients, `X-Robots-Tag`, 503 instead of 502).

## 4. The engine

Done: docker sites behind V1's walls at their own paths (cap-drop,
no-new-privileges, pids, cpus, cpu weight, user, HOST=0.0.0.0 inside, house
label and reaper, logs after adoption), `middleware exec`, `needs`,
`notice`, `hold.mode`/`until`; proven against a real rootless daemon.

Left:
- Task runs re-adopted after a restart; `health.expect_status`.
- cgroups for sites and conversations (memory and CPU for process sites).
- `group` (panel grouping).
- Rename a site; archive as one file with restore and download; download a
  site folder; screenshots.
- One deploy implementation (`internal/sites/deploy.go` and
  `internal/deploy` are two).

## 5. The agent conversation

Done: two-hour cap per turn; the MIDDLEWARE_CHAT_ID mark, with the
conversation's processes listed and killed one by one or all (setsid
escapes included); deny rules that hold (`//path`, as Claude Code reads
them), turns with permissions skipped so Bash works, the machine's keys and
dangerous commands denied, linked and nested site folders; container sites
told to use `middleware exec`.

Done too: several conversations per site; the site door (`/api/v1/site`,
site tokens, `claude_api` levels, daily cap, the socket for containers,
`claude: true`); cost per model and turns per day; loans (`chat.reads`).

Left:
- Prove the site door against the real CLI (the tests use a fake
  provider): ask, JSON, resume, and that `none` cannot read and `read`
  cannot write.
- The site door's conversations listed in the panel (they are kept and
  costed, but only reachable by name today).
- Attachments.
- Several conversations per site, messages while a turn runs, subagent
  threads, push when a turn ends, picking turns back up after a restart.
- The `middleware` conversation in the panel; Gemini's jail beyond the
  audit (it runs --yolo).
- Account usage meter; cost per project and day in the panel.
- `/docs/api` human pages, with a test that keeps them in step with routes.

## 6. The guard

- Rules for credentials (`~/.ssh`, `/proc/*/environ`), download-and-run
  (`curl | sh`), credential sweeps, with the own-HOME exemption.
- Scan site logs (`MatchLog`, ignoring 4xx paths); sweep every 3 s.
- Call the agent on an incident once it is contained.
- Daily `npm audit` (critical and new), a dependencies screen, `vulnerable`
  on publish (docs claim it).
- Push notifications (web push, VAPID) for intrusions, criticals, repo
  failures.
- Browser reaper: respect open CDP connections, 30 min, a JSONL log, a cap
  for browsers started by conversations.
- Log rotation and size cap; `SIGUSR1` diagnostics; resource episodes and
  history; repair after repeated health failures.

## 7. The panel

- Per-site: incidents tab, live traffic, worker controls, manifest history
  and restore, run a command, cache/IP/security screens.
- Archived sites list, restore, download; bulk delete.
- Service door with scoped tokens (restart-only, for apps).
- Repos list and pull watcher.
- Holding page catalogue; Spanish catalogue next to `en.json`.
- Installable app (web manifest, service worker for push).

## 8. Apps

- The app layer inside the process, then the Browser (Chromium over CDP),
  then Mask (the site factory).
