# The embedded skill

`internal/chat/skills/middleware/SKILL.md` is what an agent knows about this
house before it touches anything: what kind of thing a request is (site,
worker, task, module), where each one goes, the manifest field by field with
recipes that parse, how a deployment by pointer works, how to run long work
from a turn without losing it, how to check a site, how a domain is
published, and the lessons that were paid for more than once.

It is written as a Claude Code / agent skill: YAML front matter with `name`
and a one-line `description`, then Markdown. The conversation manager
installs it into every conversation's `HOME` before each turn, for the
per-site conversation (working directory: the site folder) and for the house
conversation (working directory: the installation root), so the agent reads
it the way it reads any skill, and an installation never has to copy it by
hand or keep it current: it ships inside the binary and changes with it.

Every manifest and task example in the skill goes through the real parsers
in `internal/chat/skill_examples_test.go`: a skill that taught a field the
schema rejects would have every agent writing manifests the engine refuses,
and the test is what says so before a release does.

To change what agents are told, edit the skill, run `go test ./internal/chat/`,
and ship the binary. A site's own `AGENTS.md` (or `CLAUDE.md`) adds what is
specific to that project; the skill is what is true for every site on every installation.
