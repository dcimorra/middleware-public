**What this changes**

One paragraph. Link the issue if there is one.

**Why**

What was wrong or missing, and what else was considered.

**Tested by doing**

Which tests you added or ran, and what they actually exercise (a real
listener, a real process, a real file). If you ran it on a server, say
what you saw.

**Checklist**

- [ ] `make lint` and `make test` pass locally
- [ ] no third-party dependency added (`go.mod` untouched)
- [ ] everything in English; comments say why
- [ ] conventional commit messages (`feat(scope): …`, `fix(scope): …`, `docs: …`)
- [ ] docs updated if an operator would see or type something different
- [ ] state changes emit an event and are written through `fsx`
