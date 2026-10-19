# Deploying

There is one way to deploy and it is the manifest's `env_from_file`: a
variable whose value is **the contents of a file**. The engine re-reads the
file every tick; when the contents change, the instance's revision changes,
and the engine hands over to a new instance born with the new value. See
[manifest.md](manifest.md#env_from_file-deploying-by-pointer).

## The pointer

```json
"run": {
  "command": ["node", "server.js"],
  "env_from_file": { "APP_DIST": ".dist-current" }
}
```

A deployment is then two things: build somewhere new while the current
build keeps serving, and write the new name into the pointer.

```sh
cd /srv/middleware/sites/example.com
git pull
npm ci && APP_DIST=.dist-2026-10-19T10-00 npm run build
echo .dist-2026-10-19T10-00 > .dist-current
```

The handover, every time:

1. a new instance starts on a fresh port, output straight to its log;
2. the engine waits until the port accepts and the health path answers 2xx
   (`run.health`, default `/`, timeout 60s);
3. the router switches to the new port: from here every new request goes
   to the new instance;
4. the old instance drains for `run.drain` (default 8s): requests already in
   flight finish; then SIGTERM, then SIGKILL.

If the new instance never becomes healthy, nothing switches: the old one
keeps serving, the event `handover.failed` says why, and the panel shows the
site with an error and the last lines of the log. Fix and write the pointer
again.

## With GitHub

Enable the `github` module ([modules.md](modules.md#github)) and add to the
manifest:

```json
"deploy": {
  "github": {
    "repository": "acme/app",
    "branch": "main",
    "build": [["npm", "ci"], ["npm", "run", "build"]],
    "pointer": ".dist-current"
  }
}
```

On a push to `main` the webhook calls the API; the module pulls, runs each
build command in the site folder with the site's environment and
`BUILD_DIR=<a fresh name>`, and on success writes that name into the
pointer. The build's output goes to the site's log. A failed build writes
nothing and emits `deploy.failed`; the running instance is untouched.

`middleware site deploy example.com` does the same by hand, and
`POST /api/v1/sites/{domain}/deploy` from anywhere with a token.

## Rollback

The previous build is still on disk. Write its name:

```sh
echo .dist-2026-10-18T16-30 > .dist-current
```

Same handover, same guarantees, backwards. Keep a few builds; a task
([manifest.md](manifest.md#tasks-tasksnamejson)) can prune the rest.

## What zero downtime means here

- **No request is refused.** The router switches between two listening
  instances; there is no moment with none. `TestHandoverDropsNoRequests`
  fires requests through a handover and counts.
- **No request is cut.** The old instance drains before it is signalled.
  Requests longer than `run.drain` are cut; raise it for a site that
  streams downloads.
- **One instance receives traffic at any moment.** Two processes exist for
  a few seconds, one receiving, one draining. An in-memory session or a
  file lock held by the old one is released when it exits; code that
  cannot tolerate a few seconds of a second process (a migration that runs
  on start, a singleton cron inside the app) must guard itself, or run as
  a worker or task instead.

### What breaks a WebSocket

A WebSocket is a TCP connection to one process. On a **site** handover the
old instance keeps its open sockets until the drain ends and then they are
closed with the process: clients reconnect and land on the new instance.
Make `run.drain` long enough for a graceful goodbye if the app sends one.

On a restart of **the middleware itself** every WebSocket through it dies at
once: the proxy owns the raw socket and it dies with the process. The
loopback listener is held by systemd and new connections queue, but an
upgraded connection cannot be handed to a new process. Restart the
supervisor when it is quiet, and tell your clients to reconnect; sites do
not restart, so the reconnect is fast.

## Docker runtime

Same pointer, same handover: the engine starts a second container, waits
for health, switches, drains and removes the old one. With `docker.build`
the image is rebuilt when the revision changes, so a pointer that names a
build directory bind-mounted into the container works the same way.

## Workers

Workers (`workers.*`) are never handed over by pointer: they restart. A
backend that must not drop connections is a site with `run`, fronted by the
one that calls it, not a worker.
