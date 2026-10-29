package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/api"
	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/jail"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// wireJails puts every site's processes in a jail of the site's own
// (internal/jail) when the machine can build one, and says so either way:
// a machine that cannot runs the sites as before, and its delegates lose
// code and conversations until it can (api.Options.Isolation).
func wireJails(a *app.App) {
	if err := jail.Available(); err != nil {
		a.Bus.Emit(events.Event{Type: "jail.off", Actor: "engine", Level: "warn",
			Message: "sites run without a jail of their own, each able to read the others: " + err.Error() + " (`middleware doctor` says how to fix it)"})
		return
	}
	setJails(a.Root)
	a.Bus.Emit(events.Event{Type: "jail.on", Actor: "engine", Message: "every site's processes, commands, builds and conversations run in a jail of the site's own"})
}

// setJails points the runtimes at the jails of root's sites.
func setJails(root string) {
	runtime.Enclose = func(cmd *exec.Cmd, spec *manifest.Spec, home string) error {
		plan := basePlan(root, spec.Domain).Write(spec.SiteDir, home)
		lendBinary(plan, root, cmd.Path)
		return jail.Wrap(cmd, plan)
	}
	agent.Enclose = func(cmd *exec.Cmd, j agent.Jail, bin string) error {
		if j.SiteDir == "" || samePath(j.SiteDir, root) {
			return nil // the house's own conversation: the operator's
		}
		domain := filepath.Base(j.SiteDir)
		if j.Config != "" {
			if err := os.MkdirAll(j.Config, 0o700); err != nil {
				return err
			}
		}
		plan := basePlan(root, domain).Write(j.SiteDir, j.Home, j.Config).Read(j.Lent...)
		// The CLIs' own state where the adapter put it (a login of the
		// operator's, a vendor's home): the turn could not run without it.
		for _, kv := range cmd.Env {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "CLAUDE_CONFIG_DIR", "CODEX_HOME", "KIMI_CODE_HOME":
				if !plan.Sees(v) {
					plan.Write(v)
				}
			}
		}
		// The site-scoped door of the API: the turn's `middleware` calls
		// are told from the operator's by the turn they run under
		// (chat.SiteOfProcess). A site's own process is not given it.
		plan.Write(api.SocketPath(root))
		lendBinary(plan, root, bin)
		return jail.Wrap(cmd, plan)
	}
}

// basePlan is what every jail of a site holds besides the site: the
// system and the installation's modules (Node, the agents' CLIs), all
// read-only, and the folders of the supervisor's PATH that are not inside
// the installation, so a command found there is found inside too.
func basePlan(root, domain string) *jail.Plan {
	plan := jail.System(domain).Read(filepath.Join(root, "modules"))
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(dir) && !within(dir, root) {
			plan.Read(dir)
		}
	}
	return plan
}

// lendBinary makes the program a jail starts visible, read-only, when
// nothing in the plan shows it: a CLI installed by hand somewhere else.
// Its folder by its real name, for a script that loads its neighbours.
// Never something of the installation's: a manifest naming another
// site's program does not lend that site's folder.
func lendBinary(plan *jail.Plan, root, bin string) {
	if bin == "" || plan.Sees(bin) || within(bin, root) {
		return
	}
	real, err := filepath.EvalSymlinks(bin)
	if err != nil || within(real, root) {
		return
	}
	plan.Read(filepath.Dir(real), bin)
}

func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}
