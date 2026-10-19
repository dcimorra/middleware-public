package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Jail is what a run may and may not touch. The core builds it; adapters
// translate it; the audit checks it. Deny holds absolute paths: every
// other site folder, the installation's data (except this site's own
// home), tasks, modules and whatever else sits at the root. The root itself
// is not listed because a deny rule on the root would also cover the site
// folder inside it: "everything but here" is expressed by listing the
// siblings, which is why the list is rebuilt for every turn.
type Jail struct {
	// SiteDir is the one folder the agent works in.
	SiteDir string
	// Home is the HOME the child gets, under data/homes/<domain>.
	Home string
	// Deny is the list of absolute paths the agent must not touch.
	Deny []string
}

// NewJail builds the jail for workdir inside root. Siblings are listed at
// call time, so a site added after this installation started is denied
// the next turn without a restart. For the installation's own conversation
// (workdir == root) the sites folder is the only thing denied: that
// conversation edits the house but never a tenant.
func NewJail(root, workdir, home string) Jail {
	root = absolute(root)
	workdir = absolute(workdir)
	if home != "" {
		home = absolute(home)
	}
	j := Jail{SiteDir: workdir, Home: home}
	if workdir == root {
		j.Deny = []string{filepath.Join(root, "sites")}
		return j
	}
	seen := map[string]bool{}
	deny := func(p string) {
		p = filepath.Clean(p)
		if p == workdir || isUnder(p, workdir) || isUnder(workdir, p) || p == home || isUnder(home, p) {
			return
		}
		if !seen[p] {
			seen[p] = true
			j.Deny = append(j.Deny, p)
		}
	}
	for _, ent := range readDir(root) {
		p := filepath.Join(root, ent)
		switch ent {
		case "sites", "data":
			for _, sub := range readDir(p) {
				q := filepath.Join(p, sub)
				if ent == "data" && sub == "homes" {
					for _, h := range readDir(q) {
						deny(filepath.Join(q, h))
					}
					continue
				}
				deny(q)
			}
		default:
			deny(p)
		}
	}
	sort.Strings(j.Deny)
	return j
}

// Violations returns the denied paths that transcript mentions. It is a
// substring search on purpose: an agent that wrote "../other.site/.env"
// would slip past a path-aware parser and a false positive costs one event
// to read, while a false negative costs a tenant's secrets.
func (j Jail) Violations(transcript string) []string {
	var out []string
	for _, d := range j.Deny {
		if strings.Contains(transcript, d) {
			out = append(out, d)
		}
	}
	return out
}

// Env is the environment base every adapter starts from: a copy of the
// supervisor's PATH and locale, HOME inside the jail, and the request's own
// variables on top. No duplicate keys, sorted, so two runs with the same
// request build byte-identical environments.
func (j Jail) Env(extra map[string]string) []string {
	env := map[string]string{}
	for _, k := range []string{"PATH", "LANG", "LC_ALL", "TZ", "TERM"} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	for k, v := range extra {
		env[k] = v
	}
	if j.Home != "" {
		env["HOME"] = j.Home
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// Prepare creates the home so a CLI that writes its config under HOME
// does not fail its first run, and refuses a jail without a working
// directory: a run with nowhere to stand would inherit the supervisor's.
func (j Jail) Prepare() error {
	if j.SiteDir == "" {
		return fmt.Errorf("jail: no working directory")
	}
	if st, err := os.Stat(j.SiteDir); err != nil || !st.IsDir() {
		return fmt.Errorf("jail: %s is not a directory", j.SiteDir)
	}
	if j.Home != "" {
		if err := os.MkdirAll(j.Home, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// absolute cleans p and makes it absolute; a deny rule with a relative
// path would match nothing the CLI sees.
func absolute(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

func isUnder(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..")
}

func readDir(p string) []string {
	ents, err := os.ReadDir(p)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}
