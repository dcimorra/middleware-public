# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem.

Use GitHub's private reporting: **Security → Report a vulnerability** on
[github.com/dcimorra/middleware-public](https://github.com/dcimorra/middleware-public/security/advisories/new).
If that is not available to you, email the maintainer at the address on the
GitHub profile with `[middleware security]` in the subject.

Say what you found, how to reproduce it, and what version
(`middleware version`) you saw it on. You will get an acknowledgement within
a few days and a fix or a reason before anything is published. Credit is
given in the release notes unless you prefer otherwise.

## What counts

Anything that lets a visitor, a site, or an agent reach beyond what
[docs/security.md](docs/security.md) says it can: the supervisor's
privileges, another site's folder, the operator's credentials, the helper's
closed list of operations, the panel without a login. Things listed under
"what is not protected" there are known and by design; a better design is a
pull request, not a report.

## Supported versions

The latest release. Fixes are not backported.
