package sites

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Validation is what a manifest would do if it were written, said before
// it is: whether the house would read it, the names it would answer and
// whether another site has them, and what a build would not find. A
// build that fails on a missing Dockerfile after minutes of waiting is
// the failure this is for.
type Validation struct {
	OK bool `json:"ok"`
	// Error is the first reason the house would refuse it, the one the
	// engine would give; empty when it reads.
	Error string `json:"error,omitempty"`
	// Problems are what would fail later even though the manifest reads:
	// a Dockerfile or a build context that is not there, a name another
	// site serves.
	Problems []string `json:"problems,omitempty"`
	// Warnings are worth reading and stop nothing (an env file missing,
	// which is often on purpose).
	Warnings []string     `json:"warnings,omitempty"`
	Routes   []RouteCheck `json:"routes,omitempty"`
}

// RouteCheck is one name the site would answer.
type RouteCheck struct {
	Host    string `json:"host"`
	Prefix  string `json:"prefix,omitempty"`
	Service string `json:"service,omitempty"`
	// Conflict is why the name would not be served: another site serves
	// it, or claims it too.
	Conflict string `json:"conflict,omitempty"`
}

// Validate checks data as the site's manifest, or the manifest on disk
// when data is empty, without writing or deploying anything. hideOthers
// keeps the other sites' names out of what it says, for a caller who
// sees one site and not the house (a delegate, the site's own agent).
func (m *Manager) Validate(domain string, data []byte, hideOthers bool) (*Validation, error) {
	if !manifest.ValidName(domain) {
		return nil, ErrNotFound
	}
	dir := m.Dir(domain)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, ErrNotFound
	}
	if len(data) == 0 {
		b, err := os.ReadFile(filepath.Join(dir, manifest.FileName))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, ErrUnmanaged
			}
			return nil, err
		}
		data = b
	}
	v := &Validation{}
	mf, err := manifest.Parse(data)
	if err == nil {
		err = mf.CheckInside(dir)
	}
	if err != nil {
		v.Error = err.Error()
		return v, nil
	}
	v.Problems, v.Warnings = filesFor(dir, mf)
	v.Routes = m.routeChecks(domain, mf, hideOthers)
	for _, r := range v.Routes {
		if r.Conflict != "" {
			p := r.Host + ": " + r.Conflict
			if r.Prefix != "" {
				p = r.Host + r.Prefix + ": " + r.Conflict
			}
			v.Problems = append(v.Problems, p)
		}
	}
	v.OK = len(v.Problems) == 0
	return v, nil
}

// routeChecks are the names mf would answer, each with whether another
// site has it now.
func (m *Manager) routeChecks(domain string, mf *manifest.Manifest, hideOthers bool) []RouteCheck {
	var out []RouteCheck
	if len(mf.Services) > 0 {
		for _, r := range mf.Routes() {
			out = append(out, RouteCheck{Host: r.Host, Prefix: r.Prefix, Service: r.Service})
		}
	} else {
		for _, h := range mf.Hosts(domain) {
			out = append(out, RouteCheck{Host: h})
		}
	}
	var owners map[string]string
	if m.Engine != nil {
		owners = m.Engine.Hosts()
	}
	// A name another site has as its folder is that site's, also when the
	// engine has no manifest of it in force.
	for i := range out {
		h := out[i].Host
		other := owners[h]
		if other == "" && h != domain {
			if _, err := os.Stat(filepath.Join(m.Root, "sites", h, manifest.FileName)); err == nil {
				other = h
			}
		}
		if other == "" || other == domain {
			continue
		}
		if hideOthers {
			out[i].Conflict = "another site of this house serves it"
		} else {
			out[i].Conflict = "served by " + other
		}
	}
	return out
}

// filesFor are the files a manifest's processes would need and do not
// find: a Dockerfile, a build context, a folder to watch (problems), an
// env file (a warning: a .env is often not there on purpose).
func filesFor(dir string, mf *manifest.Manifest) (problems, warnings []string) {
	type unit struct {
		field string
		run   *manifest.Run
	}
	var units []unit
	if mf.Run != nil {
		units = append(units, unit{"run", mf.Run})
	}
	for _, n := range mf.ServiceNames() {
		units = append(units, unit{"services." + n + ".run", mf.Services[n].Run})
	}
	names := make([]string, 0, len(mf.Workers))
	for n := range mf.Workers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w := mf.Workers[n]
		units = append(units, unit{"workers." + n, &manifest.Run{Cwd: w.Cwd, EnvFiles: w.EnvFiles, Runtime: w.Runtime, Docker: w.Docker}})
	}
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(dir, rel))
		return err == nil
	}
	for _, u := range units {
		r := u.run
		if r.Cwd != "" && !exists(r.Cwd) {
			problems = append(problems, fmt.Sprintf("%s.cwd: %s is not there", u.field, r.Cwd))
		}
		for _, f := range r.EnvFiles {
			if !exists(f) {
				warnings = append(warnings, fmt.Sprintf("%s.env_files: %s is not there (read as empty)", u.field, f))
			}
		}
		if d := r.Docker; d != nil {
			if d.Build != "" && !exists(d.Build) {
				problems = append(problems, fmt.Sprintf("%s.docker.build: %s is not there (the build context is the site folder)", u.field, d.Build))
			}
			for _, w := range d.Watch {
				if !exists(w) {
					problems = append(problems, fmt.Sprintf("%s.docker.watch: %s is not there", u.field, w))
				}
			}
			for _, vol := range d.Volumes {
				src, err := manifest.VolumeSource(vol)
				if err == nil && !src.Named && !filepath.IsAbs(src.Path) && !exists(src.Path) {
					warnings = append(warnings, fmt.Sprintf("%s.docker.volumes: %s is not there yet (docker makes it as root)", u.field, src.Path))
				}
			}
		}
	}
	return problems, warnings
}
