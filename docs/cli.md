# The command line

One binary, many subcommands. `middleware help` lists what the build you have
registers; this page describes the intended set. Everything that is not the
supervisor itself talks to it through the unix socket `data/api.sock` (no
credentials: whoever can open it owns the files) and so needs the service
running, or `--root` pointing at the installation when it is not the
default.

Common flags: `--root <dir>` (default `$MIDDLEWARE_ROOT`, then the current
directory if it holds `sites/`, then `/srv/middleware`), accepted before or
after the verb. The commands that go through the API also take `--json`.

Two kinds of command: the ones that talk to the running supervisor over the
socket (`site`, `events`, `system`, `token`, `setup`, `agents`), and the ones that
work on the files under the root and need no running process (`modules`,
`inbound`, `waf`, `guard`, `tasks`, `chat`, `skill`, `vpn`); the supervisor notices
what the second kind wrote on its next look. What the CLI does not cover is
in the API ([api.md](api.md)) and the panel.

## The process

| command | what |
| --- | --- |
| `serve [--root]` | run the supervisor. What `middleware.service` runs; from a shell it serves until Ctrl-C. Takes the listener from systemd when `LISTEN_FDS` is set. |
| `helper --socket <path> --allow-uid <user> --allow-gid <group> [--socket-group]` | the privileged helper. Runs as root under `middleware-helper.service`; answers a closed list of operations on the socket. Not meant to be run by hand. |
| `helper ping [--socket]` | check the helper answers and print its protocol and build |
| `version` | print the version the binary was built with |
| `help` | list the commands |

## Installation

| command | what |
| --- | --- |
| `install [--root] [--user] [--no-helper] [--no-start] [--yes]` | put middleware on this machine; one line per step, re-runnable. [install.md](install.md) |
| `install --requirements [--browser yes]` | only the house's requirements: packages, docker and its group, nvm with the LTS, fail2ban, a headless Chromium with `--browser yes`; no unit, binary or restart. What Sistema → "The house's requirements" runs through the helper |
| `uninstall [--yes] [--keep-binary]` | stop and remove the units. Keeps the root directory and the sites. |
| `doctor` | binary, units, loopback socket, helper, root directory, reserved ports, docker, wg, disk: one line each, exit 1 if any fails |
| `setup [--password]` | create the operator: prints a one-time code to enter in the panel, or asks for the password on the terminal with `--password`. Refuses once an operator exists. |
| `setup --join <code> [--endpoint ip:port]` | join the network another node's invitation was made in, through the running service: no domain and no operator of its own, managed from the network's panels. What `install --join` runs. |

## Sites

`middleware site …` mirrors `/api/v1/sites` ([api.md](api.md)).

| command | what |
| --- | --- |
| `site list` | every folder under `sites/`, managed or not, with mode, port and state |
| `site show <domain>` | the site object: manifest, instance, workers |
| `site add <domain> [--template static\|node\|go\|docker] [--manifest file.json\|-]` | a folder with a `site.json` |
| `site start <domain>` / `site stop <domain>` | clear or set `disabled` in the manifest |
| `site restart <domain>` / `site restart --all` | force a handover; `--all` after an upgrade, see [upgrade.md](upgrade.md) |
| `site hold <domain> [--title t] [--message m]` / `site unhold <domain>` | the holding page instead of the process |
| `site logs <domain> [-f] [--lines 200] [--worker name]` | the instance's output |
| `site deploy <domain>` | run `deploy.github.build` and write the pointer, [deploy.md](deploy.md); through the github module's runner when it is enabled |
| `site rm <domain> [--archive]` | remove, or move to `data/archive/` |

## Everything else

| command | what |
| --- | --- |
| `events [-f] [--since] [--type prefix] [--subject domain]` | the audit trail; `-f` streams |
| `system` | version, root, uptime, listeners, load, memory, disk |
| `system restart` | SIGTERM to the supervisor; systemd restarts it, sites keep running |
| `token list` / `token add <name>` / `token rm <id>` | API tokens; the token is shown once |
| `modules` (or `modules list`) / `modules verify <key> k=v…` / `modules enable <key> k=v…` / `modules disable <key>` / `modules forget <key>` | [modules.md](modules.md); `enable` verifies before it saves and prints one line per check; a value of `@file` or `-` reads the value from a file or stdin so a token never sits in the shell history. `forget` disables and deletes the credentials |
| `inbound status` / `inbound open <door>` / `inbound close <door>` | the three doors (`tunnel`, `ports`, `local`) and the certificates on disk. Moving domains and renewing is `POST /inbound/move` and `POST /inbound/certificates/{domain}/renew` in the API |
| `site publish <domain> [--door d]` / `site unpublish <domain>` / `site published` | put every name of a site on the internet, take them back, and list what the house published with each name's state |
| `migrate [--sites dir] [--apply]` | convert V1 manifests; see [upgrade.md](upgrade.md) |
| `import --to <url> --token <token> [--domain d] [--command …] [--stop …] [--start …] <folder>` | run where the site is now: move one site into a V2 house with the token its panel made (Sites → Import a site), from a V1 house (`<V1 root>/sites/<domain>`) or any app folder on any server; see [upgrade.md](upgrade.md) |
| `inbound local [on\|off] [--http addr] [--dns addr]` | the development bench: plain HTTP and a name server for the sites on this machine; prints the resolver snippet. See [install.md](install.md#local-development-bench) |
| `waf list` / `waf block <ip> [duration] [reason]` / `waf unblock <ip>` | the block list, [security.md](security.md); the duration defaults to one day. Under-attack mode (`attack` in the manifest, [manifest.md](manifest.md)) and the traffic are `/waf/under-attack` and `/waf/traffic` in the API |
| `guard incidents` | what the guard found and has not been dismissed; dismissing is `POST /guard/incidents/{id}/dismiss` |
| `tasks` (or `tasks list`) / `tasks run <name>` | scheduled work, [manifest.md](manifest.md#tasks-tasksnamejson); `run` waits and prints the log's path. The log itself is `GET /tasks/{name}/log` |
| `agents [--json]` | the providers, whether each can run, and the capacity: how many turns run of the cap, per provider, and which conversations hold them (`GET /agents`) |
| `chat [--model m] <domain> <text…>` / `chat middleware <text…>` | a turn with the site's agent; progress is printed from the event stream |
| `skill install [dir]` | write the skill the conversations run with (embedded in the binary, installed into every conversation's HOME before each turn) to `<root>/.claude/skills/middleware/`, or to `dir`, so Claude Code run by hand on the server loads it too. Needs no running process |
| `deps status` / `deps check` / `deps install <claude\|agy>` | the agent CLIs under `modules/node`: what runs, the latest published, install or update now. The daily check does this on its own ([modules.md](modules.md)) |
| `vpn status [--json]` / `vpn peer add <name>` / `vpn peer remove <name>` | the WireGuard door for the panel; `--socket` names the helper's socket. "VPN only" is `POST /modules/vpn/only` in the API |
| `install` / `uninstall` / `doctor` | above |

## Exit codes

`0` done, `1` the command failed and said why on stderr, `2` bad usage.
`doctor` returns `1` when any check fails so it can sit in a cron line.

## Environment

| variable | effect |
| --- | --- |
| `MIDDLEWARE_ROOT` | the root when `--root` is not given |
| `MIDDLEWARE_SECRET_KEY` | key for the credentials store; otherwise `data/modules-key` |
| `LISTEN_FDS` | set by systemd socket activation; `serve` takes fd 3 as its listener |
