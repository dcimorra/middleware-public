# Roadmap

V2 replaces V1 only when it does everything V1 does. This file is the one
list of what is still missing, by area; what is done is summarised at the
end. Each feature is rebuilt the V2 way (its packages, its hooks, its
manifest): V1 is read for what it does and what it learned, never for code
to copy. A line leaves this file in the commit that makes it true, with the
test that proves it. `middleware migrate` names the section here that
unblocks each V1 site it cannot convert yet.

## Before cutting over from V1

- **The two V1 sites still blocked**: one uses `cache` rules (§2), one is
  VPN-only (§2). Every other production manifest converts.
- **Prove what the tests only fake**:
  - the site door against the real Claude CLI: ask, JSON, resume, and that
    `none` cannot read and `read` cannot write (§5);
  - the agents' cgroup on a real unit with `Delegate=` (the tests write a
    cgroup-shaped folder) (§4).
- **CI visible from the conversations**: a read-only Actions token so CI
  results can be checked after a push, not only reproduced locally.
- **The cutover itself**, written as a runbook and rehearsed on a copy:
  install V2 beside V1, `middleware migrate --apply` on a copy of `sites/`,
  move the tunnel's ingress to V2's listener, watch, and the way back
  (`site.v1.json` is kept beside each converted manifest).

## 2. Getting a site online

- House response cache (manifest `cache` modes), behind the door.
- VPN-only sites and the VPN's own DNS.
- Cloudflare extras: edge purge and cache rules, the DNS record editor, the
  Access app list, narrowing a wide token; domain search and prices.

## 3. The door

- A brake on site password checks (per domain, zone and address) and at
  most two key derivations at once.
- An unreadable CIDR rejects the manifest instead of being skipped.
- The `security` block: bot families, browser lists, country allow lists,
  per-site paths, built-in probe paths (`/.env`, `/.git`…), recent blocks.
- Attack mode with proof of work, a key that survives restarts, `skip`
  paths, `until`, counters, and panel exemptions.
- Panel access: passkeys (docs/security.md claims them), passkey-only, a
  panel IP allow list, emergency open/close from the shell, CSRF across
  sibling subdomains.
- A visitor page on panic.

## 4. The engine

Nothing of V1's engine is missing; only the live proof of the agents'
cgroup above.

## 5. The agent conversation

- Push when a turn ends (needs §6's notifications).
- A human-readable `/docs/api` page in the panel (today: `docs/api.md`,
  kept in step with the routes by a test, and `/api/v1/openapi.json`).

## 6. The guard

- Rules for credentials (`~/.ssh`, `/proc/*/environ`), download-and-run
  (`curl | sh`) and credential sweeps, with the own-HOME exemption.
- Scan site logs (V1's `MatchLog`, ignoring 4xx paths); sweep every 3 s
  rather than 30.
- Call the agent on an incident once it is contained.
- A daily `npm audit` (critical and new), a dependencies screen, and
  `vulnerable` on publish (docs/security.md claims it).
- Push notifications (web push, VAPID) for intrusions, criticals and repo
  failures.
- The browser reaper: respect open CDP connections, 30 minutes, a JSONL
  log, a cap for browsers started by conversations.
- Log rotation and a size cap; `SIGUSR1` diagnostics; resource episodes and
  history; repair after repeated health failures (manifest `autofix`).

## 7. The panel

- Per site: an incidents tab, live traffic, worker controls, manifest
  history and restore, run a command, cache/IP/security screens.
- Bulk delete.
- A service door with scoped tokens (restart-only, for apps like Mask).
- A repos list and a pull watcher.
- A Spanish catalogue beside `en.json`.
- An installable app (web manifest, service worker for push).

## 8. Apps

- The app layer inside the process (manifest `app`), then the Browser
  (Chromium over CDP), then Mask (the site factory).

## Done

- **Security fixes from the first review**: manifest paths cannot leave the
  site folder (links included); static files are not served through a link
  out; one rule for trusting the edge (`internal/edge`, Cloudflare's ranges
  included); route rules on the cleaned, case-folded path; access fails
  closed; no panic when a site loses its `run`; adoption checks the pid's
  start time.
- **§1 Migration**: `middleware migrate` (report, `--apply`, `site.v1.json`
  backup) and the manifest fields V1 sites use: title, aliases, `www`,
  `env_files`, a written port, continuous health with `interval` and
  `expect_status`, workers with an assigned port and re-adoption, zones
  with their own password, `deny`, `message`, projects without a domain,
  `group`, `needs`, `notice`, hold modes and `until`, `container`,
  `claude_api`, `claude`, `chat.reads`.
- **§2 Getting online**: one publisher for creation, deletion and moves;
  every name published and certified; the door for new sites and the
  public address; dns-01 through Cloudflare; `cloudflared` run, re-adopted
  and watched by the supervisor. Proven live on 20 October 2026 against the
  real API with a throwaway tunnel and name.
- **§3 Visitor pages**: V1's seven designs, starting vs not answering, hold
  modes, JSON for API clients, 503 instead of 502.
- **§4 Engine**: docker sites behind V1's walls at their own paths
  (proven on a rootless daemon), `middleware exec`, `needs`, task runs
  re-adopted and stoppable, agent turns in a cgroup of their own, rename,
  faithful archives with restore and download, a site folder as a zip,
  site pictures (proven with headless Chromium), one deploy runner.
- **§5 Agents**: a jail that holds, two-hour turns, the processes a
  conversation leaves listed and killed, several conversations per site,
  the site door, loans, attachments, queued messages, turns that survive a
  supervisor restart, subagent threads, the house's conversation, usage
  windows and cost per project, day and model, Gemini's shell jailed,
  `docs/api.md` checked against the routes both ways.
