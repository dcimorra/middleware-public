# Upgrading

The binary is swapped, the supervisor restarts, the sites keep running.
That is the whole procedure; what follows is why it is safe and what to do
after.

## From the panel

System → Updates says which release runs and which is the newest, and
**Update now** installs it: the privileged helper downloads it from this
repository, checks its sha256, replaces `/usr/local/bin/middleware` and
both services restart; the sites keep running. **Update automatically**
(off by default; `settings.json` `update.auto`) does the same once a day.
An installation running a pre-release is offered pre-releases; one on a
stable release, stable ones only. Without the helper (`--no-helper`, a
container) the panel says to use the procedure below.

## The procedure

```sh
curl -fsSL https://raw.githubusercontent.com/dcimorra/middleware-public/main/scripts/install.sh | sh
```

or, with a binary you built or downloaded yourself:

```sh
./middleware install
```

The installer compares the binary's hash with the installed one, copies the
new one into `/usr/local/bin/middleware`, rewrites the units if the
templates changed (then `daemon-reload`), and restarts
`middleware-helper.service` and `middleware.service`. It does **not**
restart `middleware.socket`: systemd keeps holding `127.0.0.1:46100`, and
connections that arrive during the restart wait in the accept queue
instead of being refused.

By hand, the same thing is:

```sh
sudo install -m 0755 middleware /usr/local/bin/middleware
sudo systemctl restart middleware-helper middleware
middleware doctor
```

## Why the sites keep running

`middleware.service` has `KillMode=process`. On restart systemd signals the
supervisor only, not its control group; the sites, which are its children,
go on serving. When the new supervisor starts it reads
`data/state/<domain>.json`, checks each pid is alive and its port listening,
and **re-adopts** the instance: the router points at it again without a
restart. Requests to a site during the few hundred milliseconds of the
supervisor's restart queue on the socket; nothing is refused.

Two things do not survive a supervisor restart:

- **Open WebSockets** through the proxy. They are raw sockets the process
  owns ([deploy.md](deploy.md#what-breaks-a-websocket)). Clients reconnect.
- **A handover in progress.** The new supervisor finds two instances for
  the site and finishes the handover: the healthy newer one wins, the other
  is drained.

## Roll the sites over

A re-adopted instance still carries the environment it was born with and
the descriptors it inherited. If the upgrade changed how instances are
started (a new variable, a different `HOME` layout, a fix in the
environment builder), running instances do not have it until they are
handed over. The release notes say when that is the case. To roll them all
over, one at a time, each through a normal zero-downtime handover:

```sh
middleware site restart --all
```

The panel shows the same as a banner after an upgrade until it is done.

## Rolling back

Install the previous binary the same way; the state files are forward and
backward compatible within a major version. Releases are at
`https://github.com/dcimorra/middleware-public/releases`; each has its
`.sha256`.

```sh
curl -fsSL -o middleware https://github.com/dcimorra/middleware-public/releases/download/v2.0.0/middleware-linux-amd64
curl -fsSL https://github.com/dcimorra/middleware-public/releases/download/v2.0.0/middleware-linux-amd64.sha256 | sed 's/ .*//' > want
sha256sum middleware | sed 's/ .*//' | diff - want && chmod +x middleware && ./middleware install
```

## Checking

```sh
middleware version
middleware doctor
middleware events --since 10m
```

`doctor` fails the `binary` line when the installed binary and the one
running the command differ in version, which is what you see when the copy
step did not happen.

## From v1

The directory layout (`sites/<domain>/` as the whole project) is the same, so
the folders stay where they are; only the manifests change shape.

```
middleware migrate --sites /path/to/sites          # report, writes nothing
middleware migrate --sites /path/to/sites --apply  # convert the ready ones
```

Each site is reported as **ready** (with notes on what changed and why
nothing is lost) or **blocked** by the V1 settings it uses that V2 cannot
honour yet. A blocked site is not touched: converting it without the setting
would publish something that looks migrated and has quietly lost a door, a
VPN rule or its container. `--apply` keeps the original as `site.v1.json`
next to the new one and never overwrites that backup, so it can be run again
as V2 gains features, and going back is renaming a file. V1 password hashes
are carried as they are (same algorithm); passwords written in the clear are
hashed. Folders V1 ignored (`_retirado-…`) are reported and skipped.

### One site at a time, into another machine

To move a V1 house's sites into a V2 house on another machine, one by one
and each reversible, the V2 panel's **Sites → Import from V1** makes a
token for that one site (two hours) and the command to paste on the V1
machine:

```sh
/tmp/middleware-v2 import --to https://panel.example.com --token mw_… /srv/v1/sites/shop.example.com
```

It converts the manifest (refusing a site V2 cannot honour yet), copies the
folder while the site serves (without `node_modules`, which is installed on
the V2 side for its machine), stops the site in V1 by writing `disabled` in
its manifest, sends what changed, and starts it in V2. A database that lives
in the folder (SQLite, with its `-wal` and `-shm`) travels with it, copied
with the site stopped. If the V2 copy does not come up, V1 is started again
as it was. The site's names stay on V1: publish it in V2 and take each name
there ("Point it here"), then archive it in the V1 panel once it has served
a while. What V1 keeps of the site under its own `data/` (conversations,
cost) stays in V1.

### From any server

The same command takes an app from a machine that is not a V1 house: a
folder anywhere, run by pm2, systemd, a container or a screen. Its manifest
is its own `site.json` when it has one (V2, or V1 converted), or worked out
from what it holds: a `Dockerfile` (in a container, its exec-form `CMD`),
`package.json`'s `start`, an `index.html` at the top or in `public/`;
otherwise `--command` says it. `--stop` and `--start` say how the app is
stopped there for the last copy and started again if the import fails;
without them it is copied running, and a database written meanwhile may
arrive torn (the command says so first).

```sh
/tmp/middleware-v2 import --to https://panel.example.com --token mw_… --domain shop.example.com \
  --stop "pm2 stop shop" --start "pm2 start shop" /var/www/shop
```

