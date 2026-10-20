# Roadmap

V2 has to do everything V1 does. It is a product of its own, not the next
version of the machine V1 runs on: that installation stays as it is, and
nothing here plans to move it. This file is the one list of what V2 still
lacks, by area; what is done is summarised at the end. Each feature is
rebuilt the V2 way (its packages, its hooks, its manifest): V1 is read for
what it does and what it learned, never for code to copy. A line leaves
this file in the commit that makes it true, with the test that proves it.
`middleware migrate` (for anyone bringing V1 manifests over) names the
section here that unblocks each one it cannot convert yet.

## Not proven yet

What the tests only fake, to be run for real:

- The site door against the real Claude CLI: ask, JSON, resume, and that
  `none` cannot read and `read` cannot write (§5).
- The agents' cgroup on a real unit with `Delegate=` (the tests write a
  cgroup-shaped folder) (§4).
- CI results readable from the conversations (a read-only Actions token),
  so a push is checked where it runs and not only reproduced locally.

## 2. Getting a site online

- House response cache (manifest `cache` modes), behind the door.
- VPN-only sites and the VPN's own DNS.
- Cloudflare extras: edge purge and cache rules, the DNS record editor, the
  Access app list, narrowing a wide token; domain search and prices.

## 3. The door

Nothing left.

## 4. The engine

Nothing of V1's engine is missing; only the live proof of the agents'
cgroup above.

## 5. The agent conversation

Nothing left.

## 6. The guard

- Rules for credentials (`~/.ssh`, `/proc/*/environ`), download-and-run
  (`curl | sh`) and credential sweeps, with the own-HOME exemption.
- Scan site logs (V1's `MatchLog`, ignoring 4xx paths); sweep every 3 s
  rather than 30.
- Call the agent on an incident once it is contained.
- A daily `npm audit` (critical and new), a dependencies screen, and
  `vulnerable` on publish (docs/security.md claims it).
- The browser reaper: respect open CDP connections, 30 minutes, a JSONL
  log, a cap for browsers started by conversations.
- Log rotation and a size cap; `SIGUSR1` diagnostics; resource episodes and
  history; repair after repeated health failures (manifest `autofix`).

## 7. The panel

- Per site: a cache screen (waits for §2's house cache).

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
- **§3 The door**: an unreadable address or CIDR in `access` rejects the
  manifest instead of being skipped; one brake on password guessing for
  the panel and every site door (per domain, zone and address, a /64 for
  IPv6), and at most two key derivations at once; passkeys (WebAuthn
  against the standard library, proven with Chromium's virtual
  authenticator), passkey-only, the panel's address lists, `middleware
  panel open|close`, same-origin checks against sibling subdomains, and
  the house's page instead of a reset connection on a panic; the `security` block
  (robot families with exceptions and an hours window, browsers,
  countries, User-Agent fragments, per-site paths, the house's probe
  paths answered 404, the last refusals per site), converted by `migrate`;
  under-attack mode as a proof of work (a key on disk, `skip`, `until`
  written down as off when it passes, served/solved counters, the panel
  under the global switch with token callers exempt), proven with node and
  a headless Chromium, and converted by `migrate`.
- **§7 Per site**: Activity (live traffic and incidents), worker restart,
  stop and start, the manifest's history kept by the engine whoever wrote
  it and restorable, run a command, and Security and Access screens;
  bulk delete, typed count and archive by default; service doors as
  scoped tokens (read, restart, deploy, optionally for one site), checked
  by the API on the route it matched, from the panel and the CLI; the
  repositories inside the sites and a pull watcher that only ever fetches
  and merges, undoing a merge that collides; an installable panel (web
  manifest, icons, a worker at the root that caches nothing and is ready
  for §6's push), with no installability errors in Chromium; a Spanish
  catalogue, kept in step with en.json by a test (keys and placeholders).
- **§4 Engine**: docker sites behind V1's walls at their own paths
  (proven on a rootless daemon), `middleware exec`, `needs`, task runs
  re-adopted and stoppable, agent turns in a cgroup of their own, rename,
  faithful archives with restore and download, a site folder as a zip,
  site pictures (proven with headless Chromium), one deploy runner.
- **Push notifications**: web push to the operator's devices, VAPID and
  aes128gcm against the standard library (RFC 8291's example reproduced
  byte for byte), for intrusions, criticals, repositories that stop
  pulling and turns that end in the panel's conversations (§5, §6).
- **§5 Agents**: a jail that holds, two-hour turns, the processes a
  conversation leaves listed and killed, several conversations per site,
  the site door, loans, attachments, queued messages, turns that survive a
  supervisor restart, subagent threads, the house's conversation, usage
  windows and cost per project, day and model, Gemini's shell jailed,
  `docs/api.md` checked against the routes both ways, and every document of
  docs/ readable in the panel from the binary that runs.
