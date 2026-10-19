# The command line

One binary, many subcommands. `middleware help` lists what the build you have
registers; this page describes the intended set. Everything that is not the
supervisor itself talks to it through the unix socket `data/api.sock` (no
credentials: whoever can open it owns the files) and so needs the service
running, or `--root` pointing at the installation when it is not the
default.

Common flags: `--root <dir>` (default `$MIDDLEWARE_ROOT`, then the current
directory if it holds `sites/`, then `/srv/middleware`).

Commands marked "(see `middleware help`)" are part of the design but were
being written by other hands when this page was; check the built-in help for
the exact flags.

## The process

| command | what |
| --- | --- |
| `serve [--root]` | run the supervisor. What `middleware.service` runs; from a shell it serves until Ctrl-C. Takes the listener from systemd when `LISTEN_FDS` is set. |
| `helper --socket <path> --allow-gid <group>` | the privileged helper. Runs as root under `middleware-helper.service`; answers a closed list of operations on the socket. Not meant to be run by hand. (see `middleware help`) |
| `version` | print the version the binary was built with |
| `help` | list the commands |

## Installation

| command | what |
| --- | --- |
| `install [--root] [--user] [--no-helper] [--no-start] [--yes]` | put middleware on this machine; one line per step, re-runnable. [install.md](install.md) |
| `uninstall [--yes] [--keep-binary]` | stop and remove the units. Keeps the root directory and the sites. |
| `doctor` | binary, units, loopback socket, helper, root directory, reserved ports, docker, wg, disk: one line each, exit 1 if any fails |
| `setup` | create the operator: prints a one-time code to enter in the panel. Refuses once an operator exists. (see `middleware help`) |

## Sites

`middleware site …` mirrors `/api/v1/sites` ([api.md](api.md)). (see
`middleware help` for flags)

| command | what |
| --- | --- |
| `site list` | every folder under `sites/`, managed or not, with mode, port and state |
| `site show <domain>` | the site object: manifest, instance, workers |
| `site create <domain> [--template static\|node\|go\|docker]` | a folder with a `site.json` |
| `site start <domain>` / `site stop <domain>` | clear or set `disabled` in the manifest |
| `site restart <domain>` / `site restart --all` | force a handover; `--all` after an upgrade, see [upgrade.md](upgrade.md) |
| `site hold <domain> [--off] [--title] [--message]` | the holding page instead of the process |
| `site logs <domain> [--lines N] [--follow] [--worker name]` | the instance's output |
| `site deploy <domain>` | run `deploy.github.build` and write the pointer, [deploy.md](deploy.md) |
| `site env <domain>` | what the running instance was born with, secrets masked |
| `site delete <domain> [--archive]` | remove, or move to `data/archive/` |

## Everything else

| command | what |
| --- | --- |
| `events [--since] [--type prefix] [--subject domain] [--follow]` | the audit trail; `--follow` streams |
| `system` | version, root, uptime, listeners, load, memory, disk |
| `system restart` | SIGTERM to the supervisor; systemd restarts it, sites keep running |
| `token list` / `token create <name>` / `token delete <id>` | API tokens; the token is shown once |
| `modules list` / `modules verify <key>` / `modules enable <key>` / `modules disable <key>` | [modules.md](modules.md); `enable` verifies before it saves and prints one line per check |
| `inbound` / `inbound move --to tunnel\|ports <domain…>` / `inbound certs` / `inbound renew <domain>` | the two doors and the certificates |
| `waf` / `waf block <ip> [--for 1h] [--reason]` / `waf unblock <ip>` / `waf under-attack [--off] [--domain]` / `waf traffic` | [security.md](security.md) |
| `guard` / `guard dismiss <id>` | rules and incidents |
| `tasks list` / `tasks run <name>` / `tasks log <name>` | scheduled work, [manifest.md](manifest.md#tasks-tasksnamejson) |
| `chat <domain> "text"` / `chat middleware "text"` | a turn with the site's agent; progress is printed from the event stream |
| `vpn` / `vpn peer add <name>` / `vpn peer delete <name>` / `vpn only [--off]` | the WireGuard door for the panel |

All of the above: (see `middleware help`).

## Exit codes

`0` done, `1` the command failed and said why on stderr, `2` bad usage.
`doctor` returns `1` when any check fails so it can sit in a cron line.

## Environment

| variable | effect |
| --- | --- |
| `MIDDLEWARE_ROOT` | the root when `--root` is not given |
| `MIDDLEWARE_SECRET_KEY` | key for the credentials store; otherwise `data/modules-key` |
| `LISTEN_FDS` | set by systemd socket activation; `serve` takes fd 3 as its listener |
