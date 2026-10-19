# Contributing

Thanks for looking. This is a small project with firm rules; they are what
keep one binary installable on a server you do not want to babysit.

## The rules

1. **No third-party dependencies in the core.** `go.mod` names none and CI
   fails the build if one appears (`go list -m all` must print one line).
   ACME, WebAuthn and the rest are written against the standard library.
   Something that genuinely needs a library becomes a *module* that drives
   an external binary with `os/exec`, never an import.
2. **English everywhere.** Identifiers, comments, tests, docs, commit
   messages, panel strings. The panel has a string catalog so other
   languages can be added without touching code.
3. **Comments say why.** What was tried, what broke, what happens if someone
   changes it back. A comment that restates the code is deleted.
4. **Conventional commits**, lower-case imperative subject, scoped when it
   helps: `feat(engine): hand over by pointer`, `fix(proxy): keep websockets
   open across reloads`, `docs: describe the manifest`, `test(acme): …`.
   One change per commit; several commits per pull request are fine.
5. **Nothing runs as root, nothing touches the real system** from a test or
   from a development session. The installer goes through an `Exec`
   interface and tests record the commands; the helper's operations are
   tested with their inputs, not against a live firewall.

## Tests

Test names are sentences: `TestARateLimitedClientGetsA429`,
`TestHandoverDropsNoRequests`. Anything that holds the house up is tested by
**doing it**: a real listener on `127.0.0.1:0`, a real process, real files in
`t.TempDir()`. A test that reads a function's code back to itself proves
nothing.

A test that needs an external service, a container daemon or root is
skipped unless an environment flag is set, and the flag is named in the
test's comment (`MIDDLEWARE_TEST_DOCKER=1`, `MIDDLEWARE_TEST_ACME=1`).

Before pushing:

```sh
make lint    # gofmt, vet, dependency check
make test    # go test -race ./...
make build
```

## What CI runs

`.github/workflows/ci.yml` on every push and pull request: `gofmt -l .`
prints nothing, `go vet ./...`, `go test -race ./...`, `go build`, and the
dependency count. `release.yml` runs on a `v*` tag: the same tests, then
amd64 and arm64 binaries with their `.sha256` files on a GitHub release.

## Where things go

- `cmd/middleware/` — argument parsing only. Each feature registers its
  subcommands and hooks from its own file's `init`.
- `internal/<package>/` — one concern per package. Read
  `docs/architecture.md` before touching `engine`, `runtime` or `proxy`.
- `docs/` — operator-facing. If a change alters what an operator sees or
  types, the doc changes in the same pull request.

Everything that changes state emits an event through `events.Bus` and
writes to disk through `fsx` (atomic). A decision that does not survive a
restart is not a decision: it goes in the manifest or in `data/`.

## Pull requests

Fill in the template. Say what you tested by doing. A pull request that
adds a dependency, a language other than English, or a path to root is
closed with a pointer to this file.
