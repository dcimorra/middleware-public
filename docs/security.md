# Security

What protects what, and what does not. Report vulnerabilities as
[SECURITY.md](../SECURITY.md) says.

## Threat model

The machine runs sites you do not fully trust (a dependency you did not
read, a build script from a repository, an agent that writes code) behind a
supervisor that you do trust, operated through a panel that must not be a
way in. The attacker is: anyone on the internet, a compromised site, or an
agent doing something it should not.

What is defended: the supervisor's privileges, the other sites, the
operator's credentials, the panel. What is not: a site's own data from that
site's own code. A site that leaks its own database leaks it; middleware is
not a sandbox for the site's logic.

## The pieces

**Unprivileged supervisor.** `middleware.service` runs as your user. It
binds 80/443 with `CAP_NET_BIND_SERVICE` and that is the only capability it
has. If the supervisor is compromised, the attacker is your user, not root.

**Closed helper.** `middleware-helper.service` is the only root process.
It listens on `/run/middleware/helper.sock` (`root:middleware`, `0660`) and
answers a **closed list of operations**: `firewall.apply`, `firewall.block`,
`firewall.unblock`, `vpn.apply`, `vpn.up`, `vpn.down`, `sysctl.reserve_ports`,
`update.install`, `requirements.install`. `requirements.install` takes one
boolean (add a headless Chromium) and nothing else: what runs is the
helper's own binary's `install --requirements`, for the user, root and
binary the installed `middleware.service` names (written by root; a unit
pointing at another binary is refused), in a transient unit, and it only
installs the house's own list of packages; it writes no unit, replaces no
binary and restarts nothing. `update.install` takes a release's name and nothing else: it
downloads that release from this repository only, checks the published
sha256, runs the binary to see it is that version, and refuses anything
not newer than what runs, so a supervisor that was broken into can neither
install a binary of its own as root nor go back to an older one.
Each validates its input (addresses parse, keys are base64 of the right
length, names match `^[a-z0-9-]+$`) and none takes a free-form string that
reaches a shell. Its unit has `ProtectSystem=strict` with exactly three
writable paths. A feature that needs root needs a new operation with its own
test, not a wider one. Inside Docker the helper is optional: the image's
entrypoint starts as root, runs the helper only when `MIDDLEWARE_HELPER=1`
(with `NET_ADMIN` granted by compose), and drops to the `middleware` user
for the supervisor either way; without it those features are absent rather
than running as root.

**Subscription sessions.** Signing in with a Claude subscription or a
Google account runs the vendor's own interactive flow on a pty with a
temporary HOME; what comes out (a long-lived token, a token file) is
stored in the encrypted credentials store like any other secret and never
shown again: the panel's "what the program printed" view blanks it. A
turn receives it in its environment (`CLAUDE_CODE_OAUTH_TOKEN`) or under
its HOME (`.gemini/antigravity-cli/antigravity-oauth-token`, mode 0600), which an agent running
there could read; a site's own process never does.

**Per-site HOME and environment.** Each site runs with `HOME` under
`data/homes/<domain>/`, an environment built from its manifest only (no
inherited variables from the supervisor, no duplicate keys), and its own
process group. A site's `~/.ssh`, `~/.npmrc` or agent login is not another
site's. Sites run as the same Unix user as the supervisor, though: see
"what is not protected".

**The jail for agents.** A conversation's agent may read and write the
site's folder and nothing above it, and no other site. The jail is enforced
by the core before anything reaches the CLI: paths are resolved and checked
against the site folder, symlinks included. The adapter is a subprocess with
no say in it.

**Credentials encrypted at rest.** Module credentials live in
`data/modules.json`, AES-256-GCM, key from `MIDDLEWARE_SECRET_KEY` or
`data/modules-key` (mode `0600`). Backups of the root are safe to store
elsewhere only if the key is stored apart. The panel password is hashed;
API tokens are stored hashed and shown once.

**The panel.** Served on the loopback listener and on the panel domain
only, never on a site's domain. Login with a password, or passkeys;
sessions are cookies with `Secure`, `HttpOnly`, `SameSite=Strict`. The
unix socket `data/api.sock` needs no credentials because its file mode is
the credential. The panel answers through every door so closing the one you
came in by cannot lock you out, and the VPN module can make it reachable
through WireGuard only.

**Password guessing.** Every password door (the panel's login, a site's,
each zone's) goes through one brake (`internal/brake`): failures are counted
per address (per /64 for IPv6), and five inside a minute lock that address
out of that door for a minute, doubling with each lock (to a day for the
panel, fifteen minutes for a site). A success forgets the count. Locked
attempts get 429 with `Retry-After` and are not checked at all. On top of
that, at most two key derivations run at once in the whole installation, so
guesses spread over many addresses queue instead of taking every site's CPU.

**Probe paths.** Every site answers 404, before its process sees the
request, to what no website serves: `.git/`, `.ssh/`, `.env*`, private keys,
`/etc/passwd`, `/proc/self/`. A site adds its own prefixes in
`security.paths`, and can close robot families, browsers and countries there
too; that part trusts the User-Agent, so it filters what identifies itself
and guards nothing.

**Under attack.** `attack` in a manifest, or in the WAF policy for the whole
installation and the panel, puts a proof of work in front of the site: the
browser spends a fraction of a second, a flood pays it on every request,
and checking costs one hash. The pass is signed with a key on disk and bound
to the client. It filters floods, not people: whoever runs JavaScript gets
through.

**WAF, in layers.** Layer 7 in the core, always on: rate limits per route
with burst, body size limits, country and user-agent policies, a password or
an allow list per route, and an "under attack" mode that challenges or
rejects. The rate is per visitor, not per address: an address and its
browser (User-Agent and Accept-Language) are one visitor, so two phones on
one home connection have a burst each. What a page brings in with it (an
image, a script, a font, or a request that only asks whether it changed)
costs a quarter and is paid by the address alone, never the visitor: a
page of three hundred files reloaded ten times is neither refused nor
blocked. A visitor past its own rate gets 429s and nothing more. The
address as a whole carries eight visitors' worth (`NATShare`), and only a
flood past that, `trips` times in a minute, blocks it: for a while, and
when the helper is present also in nftables (`firewall.block`), so a flood
stops at layer 3 and never reaches Go. That block closes every site at
once, which is why volume alone from a person never earns it; exploits,
scanners and failed logins still do (the bans). Blocks have a TTL and are
written down, so a restart neither forgets nor keeps them forever.

The body limit (`max_body`, 20m by default) is for what visitors send. It
does not apply to a site's bundle coming into this house (`PUT
/api/v1/relocate/{domain}` and its `/delta`, on a host that is not a site):
a move or an import is hundreds of megabytes on purpose, and the API behind
authorizes it (the network's secret or a one-site import token). A body cut
by the limit halfway through, as a streamed one is, answers 413 naming the
limit and where to change it.

The operator's own session on the panel's domain is not rate-limited nor
counted toward a block: it is the panel's own traffic (the dashboard, the
bar asking every node, the stream), which on a phone passed the limit in a
minute and blocked the operator for ten. It is recognised by its cookie,
not its address, which on a phone changes; and the address it came from is
trusted for a week, as after a login.

**Guard.** Rules written in code (a site writing outside its folder, a
process spawning a browser, `npm audit` findings, a port that should not be
listening) produce incidents; the rule decides and acts, and only then may a
model be asked to explain. Who presses the button is never a model.

**Inbound.** The tunnel door exposes no port; the ports door exposes 80/443
with certificates obtained for the domains this installation serves and no
other. Headers from the edge (`X-Forwarded-For`, `CF-Connecting-IP`) are
trusted only from the door they belong to.

## Bans (fail2ban)

Inside the WAF, the first layer every request crosses. A request carrying
an exploit that is wrong on any site (Log4Shell's lookup in the target or
a header, a climb out of the folder to `/etc/passwd`, PHP's stream
wrappers, phpunit's `eval-stdin.php`, the routers' RCEs, command
injection, Shellshock) is refused and bans its address at once. What
scanners ask every server for (`/.env`, `/.git/`, `/wp-login.php`,
`/phpmyadmin`, a `.php` on a site without PHP…) counts as one attempt
only when the site answered with an error, so a WordPress's visitors are
never counted; a wrong panel password counts too. Five attempts in ten
minutes ban the address: in the WAF's block list (kept on disk) and in the
kernel through the helper's nftables set, for an hour the first time and
four times longer at every repeat, up to thirty days. Lifting a ban lifts
it in the kernel too, and what is still banned goes back into the kernel
when the supervisor starts (a reboot empties the kernel's set).

SSH is the one door the house does not see, so the installer gives it
fail2ban with the same numbers and exceptions; each of its bans is written
into the same block list (reason `fail2ban sshd`), so the panel lists it
with the others. fail2ban lifting it lifts it in the house; lifting it in
the panel lifts the house's block, while fail2ban keeps SSH closed to that
address until its own time is up (`fail2ban-client unban <ip>` ends it).

Never banned: loopback, the private and CGNAT ranges (the VPN's, the
mesh's), the allow list, and an address the operator logged in from in
the last week. Behind the tunnel the address is the edge's
`CF-Connecting-IP`; the kernel cannot cut those (they arrive from
cloudflared), so for them the ban is the WAF's 403.

## What is not protected

- **Sites share a Unix user.** Isolation between sites is HOME, environment,
  process group and the jail for agents, not a user boundary. A site that
  reads `../other-site/` can. For hard isolation, run that site with
  `run.runtime: "docker"`: it then sees only its own folder.
- **The supervisor can read everything in the root.** It has to. A bug in
  the supervisor is a bug with your user's rights.
- **Secrets in a site's `.env` are plain files** in the site folder, readable
  by anything running as your user. Encryption at rest covers module
  credentials, not site files.
- **Docker is root-equivalent.** A site in the docker runtime is isolated
  from the others, but the `docker` group grants the supervisor root on the
  host through the daemon. That is Docker's model, not ours.
- **The loopback listener trusts the machine.** Anything that can reach
  `127.0.0.1:46100` can reach the panel's login page. Use the VPN module or
  a tunnel that authenticates (Cloudflare Access) if other people have
  shells here.
- **A model may be wrong.** Agents act inside the jail with the site's
  rights; a conversation can still delete the site's files, commit nonsense
  or spend money. The git history is the undo.
- **No protection against the operator.** Tokens and the socket give full
  control; there are no roles.
