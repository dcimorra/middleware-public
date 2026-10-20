package manifest

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Spec is what an instance is born from: the manifest with every variable
// and pointer resolved. Two specs with the same Revision would start the
// same process, so the engine compares revisions and nothing else.
type Spec struct {
	Domain  string
	SiteDir string
	Command []string
	Cwd     string
	Env     map[string]string // without PORT; the runtime adds it last
	Health  Health
	Drain   Duration
	Restart Restart
	Runtime string
	Docker  *Docker
	// Port is the written port, or 0 for one the engine assigns.
	Port int
	// Mounts are more folders a container sees at their own path (the
	// site door's socket folder).
	Mounts   []string
	Revision string
}

// Rehash recomputes the revision after the engine added to the spec.
func (s *Spec) Rehash() { s.Revision = s.hash() }

// Resolve turns a manifest into the spec of the instance it describes.
// `$PORT` is left in place: the port is chosen at start, and the revision
// must not change just because the engine picked another port.
func Resolve(domain, siteDir string, m *Manifest) (*Spec, error) {
	if m.Run == nil {
		return nil, fmt.Errorf("%s: no run section", domain)
	}
	vars := map[string]string{"DOMAIN": domain, "SITE_DIR": siteDir}
	env := map[string]string{}
	for _, f := range m.Run.EnvFiles {
		fileEnv, err := readEnvFile(filepath.Join(siteDir, Expand(f, vars)))
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("%s: env_files: %s: %w", domain, f, err)
		}
		for k, v := range fileEnv {
			env[k] = v
		}
	}
	for k, v := range m.Run.Env {
		env[k] = Expand(v, vars)
	}
	for k, file := range m.Run.EnvFromFile {
		v, err := os.ReadFile(filepath.Join(siteDir, Expand(file, vars)))
		if err != nil {
			// A missing pointer file is not a crash: the variable is set
			// empty and the event stream says so, because the alternative
			// (refusing to start) would take a site down over a deploy
			// script that has not run yet.
			env[k] = ""
			continue
		}
		env[k] = strings.TrimSpace(string(v))
	}
	cmd := make([]string, len(m.Run.Command))
	for i, a := range m.Run.Command {
		cmd[i] = Expand(a, vars)
	}
	cwd := siteDir
	if m.Run.Cwd != "" {
		cwd = filepath.Join(siteDir, m.Run.Cwd)
	}
	s := &Spec{
		Domain:  domain,
		SiteDir: siteDir,
		Command: cmd,
		Cwd:     cwd,
		Env:     env,
		Runtime: m.Run.Runtime,
		Docker:  m.Run.Docker,
		Drain:   m.Run.Drain,
		Port:    m.Run.Port,
	}
	if s.Runtime == "" {
		s.Runtime = "process"
	}
	if m.Run.Health != nil {
		s.Health = *m.Run.Health
	}
	if m.Run.Restart != nil {
		s.Restart = *m.Run.Restart
	}
	s.Revision = s.hash()
	return s, nil
}

// Expand replaces $NAME and ${NAME} for the variables the manifest may use.
// Unknown names are left as they are: a site's own `$HOME` in a command must
// reach the shell-less exec untouched, and expanding it to "" would be a
// silent change of meaning.
func Expand(s string, vars map[string]string) string {
	return os.Expand(s, func(name string) string {
		if v, ok := vars[name]; ok {
			return v
		}
		return "$" + name
	})
}

// WithPort expands $PORT in the command and env of a resolved spec.
func (s *Spec) WithPort(port int) (cmd []string, env map[string]string) {
	p := strconv.Itoa(port)
	vars := map[string]string{"PORT": p, "DOMAIN": s.Domain, "SITE_DIR": s.SiteDir}
	cmd = make([]string, len(s.Command))
	for i, a := range s.Command {
		cmd[i] = Expand(a, vars)
	}
	env = make(map[string]string, len(s.Env)+4)
	for k, v := range s.Env {
		env[k] = Expand(v, vars)
	}
	return cmd, env
}

func (s *Spec) hash() string {
	h := sha256.New()
	w := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	}
	w(s.Domain, s.Cwd, s.Runtime, strconv.Itoa(s.Port))
	w(s.Command...)
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w(k, s.Env[k])
	}
	w(s.Mounts...)
	if s.Docker != nil {
		w(s.Docker.Image, s.Docker.Build, s.Docker.Memory, s.Docker.User,
			strconv.Itoa(s.Docker.Pids), strconv.FormatFloat(s.Docker.CPUs, 'f', -1, 64), strconv.Itoa(s.Docker.CPUWeight))
		w(s.Docker.Volumes...)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// readEnvFile parses KEY=VALUE lines, with # comments, optional `export `,
// and single or double quotes around the value. It is the subset every
// .env loader agrees on; anything fancier belongs in the site's own code.
// A file ending in .json is an object of strings instead, which is what
// tools that write secrets tend to produce.
func readEnvFile(path string) (map[string]string, error) {
	if strings.HasSuffix(strings.ToLower(path), ".json") {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
		out := map[string]string{}
		for k, v := range raw {
			if sv, ok := v.(string); ok && validEnvKey(k) {
				out[k] = sv
			}
		}
		return out, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if validEnvKey(k) {
			out[k] = v
		}
	}
	return out, sc.Err()
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".site.json.*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
