# CLAUDE.md

Guidance for Claude Code when working in this repository.

## What this is

`middleware` is a single Go process that sits in front of every site on a
machine. It reads the `Host` header and either serves `sites/<domain>/` or
forwards to the process that site's manifest describes. One process per site,
always; deployments hand over by pointer with zero dropped requests. Read
[`docs/architecture.md`](docs/architecture.md) before touching the core and
[`docs/manifest.md`](docs/manifest.md) before touching anything a manifest
describes.

## Non-negotiable rules

1. **No third-party dependencies in the core.** `go.mod` names none and CI
   fails if one appears. ACME, WebAuthn, CBOR and friends are written against
   the standard library. Anything that genuinely needs a dependency is a
   *module* that installs an external binary when enabled (cloudflared,
   wireguard-tools, an agent CLI), never an import.
2. **Everything in English.** Identifiers, comments, tests, commit messages,
   docs, panel strings. The panel has a string catalog (`internal/panel/ui/i18n/`)
   so other languages can be added without touching code.
3. **Conventional commits**, lower-case subject, imperative, scoped when it
   helps: `feat(engine): hand over by pointer`, `fix(proxy): keep websockets
   open across reloads`, `docs: describe the manifest`.
4. **A comment says why, not what.** What was tried before, what broke, what
   happens if someone changes it back.
5. **Anything that holds the house up is tested by doing it, not by reading
   it.** A new path that all traffic crosses needs a test that crosses it.

## Things that are not obvious

- **Never hand an `io.Writer` to `exec.Cmd`** for a supervised process. `Wait`
  hangs while a grandchild keeps the pipe, and a pipe with no reader kills the
  child with SIGPIPE once the parent goes. Output goes straight to a file.
- **`KillMode=process` in the unit.** systemd would otherwise kill the whole
  control group on stop and take the sites with it. Sites outlive restarts
  and are re-adopted when the supervisor comes back.
- A child's environment is built **without duplicate keys** and `PORT` goes
  **last**: glibc reads the first occurrence, Node the last.
- The privileged helper (`middleware-helper`) has a **closed list of
  operations**. The supervisor never runs as root. If a feature needs root,
  it needs a new operation in the helper with its own test, not a wider one.
- A decision that does not survive a restart is not a decision: stopped,
  held, jailed, all of it is written to the site's manifest.

## Before pushing

```bash
gofmt -l .            # must print nothing
go vet ./...
go test -race ./...
go build -o middleware ./cmd/middleware
```
