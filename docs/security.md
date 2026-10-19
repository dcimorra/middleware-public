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
`firewall.unblock`, `vpn.apply`, `vpn.up`, `vpn.down`, `sysctl.reserve_ports`.
Each validates its input (addresses parse, keys are base64 of the right
length, names match `^[a-z0-9-]+$`) and none takes a free-form string that
reaches a shell. Its unit has `ProtectSystem=strict` with exactly three
writable paths. A feature that needs root needs a new operation with its own
test, not a wider one. Inside Docker there is no helper and those features
are absent rather than running as root.

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

**WAF, in layers.** Layer 7 in the core, always on: rate limits per route
with burst, body size limits, country and user-agent policies, a password or
an allow list per route, and an "under attack" mode that challenges or
rejects. An address that trips a limit is blocked for a while; when the
helper is present the block is also pushed to nftables (`firewall.block`)
so a flood stops at layer 3 and never reaches Go. Blocks have a TTL and are
written down, so a restart neither forgets nor keeps them forever.

**Guard.** Rules written in code (a site writing outside its folder, a
process spawning a browser, `npm audit` findings, a port that should not be
listening) produce incidents; the rule decides and acts, and only then may a
model be asked to explain. Who presses the button is never a model.

**Inbound.** The tunnel door exposes no port; the ports door exposes 80/443
with certificates obtained for the domains this installation serves and no
other. Headers from the edge (`X-Forwarded-For`, `CF-Connecting-IP`) are
trusted only from the door they belong to.

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
