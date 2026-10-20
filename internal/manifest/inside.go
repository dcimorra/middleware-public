package manifest

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// CheckInside refuses a manifest that names a path outside its site folder.
//
// The manifest is written by the site's own agent conversation as well as
// by people, and the agent is jailed to the folder: a `static.root` of
// "../other.example" would publish a neighbour's files, an `env_files` of
// "../other.example/.env" would hand its secrets to this site's process,
// and a docker volume of "/:/host" would mount the machine. V1 learned this
// as `checkInside`; the rule lives where the manifest is read, so no caller
// can forget it.
//
// Symlinks are resolved when the named path exists, because that is how
// whoever opens it will see it: `public` can be a link to anywhere.
func (m *Manifest) CheckInside(siteDir string) error {
	if siteDir == "" {
		return nil
	}
	if err := m.checkProject(filepath.Base(siteDir)); err != nil {
		return err
	}
	base := filepath.Clean(siteDir)
	real := base
	if r, err := filepath.EvalSymlinks(base); err == nil {
		real = r
	}
	inside := func(field, p string) error {
		if p == "" {
			return nil
		}
		full := p
		if !filepath.IsAbs(full) {
			full = filepath.Join(base, full)
		}
		full = filepath.Clean(full)
		resolved := full
		if r, err := filepath.EvalSymlinks(full); err == nil {
			resolved = r
		}
		if !(within(full, base) || within(full, real)) || !(within(resolved, real) || within(resolved, base)) {
			return fmt.Errorf("%s: %q leaves the site folder", field, p)
		}
		return nil
	}
	vars := map[string]string{"SITE_DIR": base}
	if m.Static != nil {
		if err := inside("static.root", m.Static.Root); err != nil {
			return err
		}
	}
	if r := m.Run; r != nil {
		if err := inside("run.cwd", Expand(r.Cwd, vars)); err != nil {
			return err
		}
		for _, f := range r.EnvFiles {
			if err := inside("run.env_files", Expand(f, vars)); err != nil {
				return err
			}
		}
		for k, f := range r.EnvFromFile {
			if err := inside("run.env_from_file."+k, Expand(f, vars)); err != nil {
				return err
			}
		}
		if d := r.Docker; d != nil {
			if err := inside("run.docker.build", d.Build); err != nil {
				return err
			}
			for _, v := range d.Volumes {
				src, err := VolumeSource(v)
				if err != nil {
					return fmt.Errorf("run.docker.volumes: %w", err)
				}
				if src.Named {
					continue
				}
				if err := inside("run.docker.volumes", src.Path); err != nil {
					return err
				}
			}
		}
	}
	for name, w := range m.Workers {
		if w == nil {
			continue
		}
		if err := inside("workers."+name+".cwd", Expand(w.Cwd, vars)); err != nil {
			return err
		}
		for _, f := range w.EnvFiles {
			if err := inside("workers."+name+".env_files", Expand(f, vars)); err != nil {
				return err
			}
		}
		for k, f := range w.EnvFromFile {
			if err := inside("workers."+name+".env_from_file."+k, Expand(f, vars)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Volume is the host side of a docker volume spec.
type Volume struct {
	// Named is a docker named volume ("data:/app/data"). The runtime
	// prefixes it with the site, because docker names are global and two
	// sites writing "data" would otherwise share one volume.
	Named bool
	Name  string
	// Path is a host path, relative to the site folder or absolute.
	Path string
	// Rest is everything after the host side: ":/container[:opts]".
	Rest string
}

var volumeNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// VolumeSource splits "host:container[:opts]". A bare "/container" (an
// anonymous volume) has no host side and is treated as named with no name.
func VolumeSource(v string) (Volume, error) {
	host, rest, ok := strings.Cut(v, ":")
	if !ok {
		if !strings.HasPrefix(v, "/") {
			return Volume{}, fmt.Errorf("%q is not host:container", v)
		}
		return Volume{Named: true, Rest: v}, nil
	}
	if host == "" || rest == "" {
		return Volume{}, fmt.Errorf("%q is not host:container", v)
	}
	if !strings.ContainsAny(host, `/\`) && host != "." && host != ".." {
		if !volumeNameRe.MatchString(host) {
			return Volume{}, fmt.Errorf("%q is not a volume name", host)
		}
		return Volume{Named: true, Name: host, Rest: ":" + rest}, nil
	}
	return Volume{Path: host, Rest: ":" + rest}, nil
}

func within(p, dir string) bool {
	if p == dir {
		return true
	}
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// checkProject refuses what a project cannot do. Said instead of ignored: a
// project whose run section silently ran nothing is the failure that looks
// like success.
func (m *Manifest) checkProject(name string) error {
	if !IsProject(name) {
		return nil
	}
	if m.Run != nil || m.Upstream != "" || m.Static != nil || len(m.Aliases) > 0 {
		return fmt.Errorf("%s is a project and answers no hostname: run, upstream, static and aliases need a folder named after a domain", name)
	}
	return nil
}
