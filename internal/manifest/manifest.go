// Package manifest defines site.json, the only state a site has. Everything
// the engine, the proxy and the panel know about a site comes from here, and
// everything they decide about it is written back here. The schema is
// described for humans in docs/manifest.md; this file is the schema for the
// compiler.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// FileName is the manifest's name inside a site folder.
const FileName = "site.json"

// Version is the only supported schema version.
const Version = 2

// Manifest is site.json.
type Manifest struct {
	Version int    `json:"version"`
	Title   string `json:"title,omitempty"`
	// Aliases are more names this site answers to, served the same way
	// and published with it.
	Aliases []string `json:"aliases,omitempty"`
	// WWW false drops the www. name an apex domain carries without writing
	// it (see Hosts).
	WWW      *bool              `json:"www,omitempty"`
	Static   *Static            `json:"static,omitempty"`
	Run      *Run               `json:"run,omitempty"`
	Upstream string             `json:"upstream,omitempty"`
	Workers  map[string]*Worker `json:"workers,omitempty"`
	Disabled bool               `json:"disabled,omitempty"`
	Hold     *Hold              `json:"hold,omitempty"`
	Access   *Access            `json:"access,omitempty"`
	WAF      *WAF               `json:"waf,omitempty"`
	Cache    *Cache             `json:"cache,omitempty"`
	Headers  map[string]string  `json:"headers,omitempty"`
	Deploy   *Deploy            `json:"deploy,omitempty"`
	Agent    *Agent             `json:"agent,omitempty"`
	App      string             `json:"app,omitempty"`
}

// Static serves files.
type Static struct {
	Root string `json:"root,omitempty"`
	SPA  bool   `json:"spa,omitempty"`
}

// Run describes the supervised instance.
type Run struct {
	Command []string          `json:"command"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// EnvFiles are read in order, a later file overriding an earlier one,
	// and `env` over all of them. A missing file is not an error: `.env`
	// is usually not committed.
	EnvFiles    []string          `json:"env_files,omitempty"`
	EnvFromFile map[string]string `json:"env_from_file,omitempty"`
	// Port is written only when something outside the house already knows
	// it. A site with a written port cannot hand over (two generations
	// cannot share it), so a change restarts it instead.
	Port    int      `json:"port,omitempty"`
	Health  *Health  `json:"health,omitempty"`
	Drain   Duration `json:"drain,omitempty"`
	Restart *Restart `json:"restart,omitempty"`
	Runtime string   `json:"runtime,omitempty"` // "process" (default) | "docker"
	Docker  *Docker  `json:"docker,omitempty"`
}

// Health is how the engine knows a new instance is ready.
type Health struct {
	Path    string   `json:"path,omitempty"`
	Timeout Duration `json:"timeout,omitempty"`
	// Interval is how often a running instance is asked, once it is up.
	Interval Duration `json:"interval,omitempty"`
}

// Restart bounds the backoff after a crash.
type Restart struct {
	MaxBackoff Duration `json:"max_backoff,omitempty"`
}

// Docker is the container runtime's share of the manifest.
type Docker struct {
	Image   string   `json:"image,omitempty"`
	Build   string   `json:"build,omitempty"` // Dockerfile path, relative to the site
	Volumes []string `json:"volumes,omitempty"`
	Memory  string   `json:"memory,omitempty"`
	// Pids caps the processes inside (default 1024). CPUs is a hard cap
	// in cores; CPUWeight is the relative share under contention (docker's
	// cpu-shares, 1024 is the default share).
	Pids      int     `json:"pids,omitempty"`
	CPUs      float64 `json:"cpus,omitempty"`
	CPUWeight int     `json:"cpu_weight,omitempty"`
	// User is uid:gid inside; empty is the right default for the daemon
	// (see runtime.Docker).
	User string `json:"user,omitempty"`
}

// Worker is an always-on process that belongs to a site and answers no
// hostname of its own. With no port, one is assigned and passed in $PORT;
// a written port is for when whoever calls the worker already knows it.
type Worker struct {
	Command     []string          `json:"command"`
	Cwd         string            `json:"cwd,omitempty"`
	Port        int               `json:"port,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	EnvFiles    []string          `json:"env_files,omitempty"`
	EnvFromFile map[string]string `json:"env_from_file,omitempty"`
	Disabled    bool              `json:"disabled,omitempty"`
	Note        string            `json:"note,omitempty"`
}

// Hold shows a page instead of the site, with the process stopped.
type Hold struct {
	Enabled bool   `json:"enabled"`
	Title   string `json:"title,omitempty"`
	Message string `json:"message,omitempty"`
}

// Access decides who gets in. Passwords are stored hashed (see
// internal/access); the panel and `middleware site password` write them.
type Access struct {
	Password      string                  `json:"password,omitempty"`
	Message       string                  `json:"message,omitempty"` // shown above the password box
	Allow         []string                `json:"allow,omitempty"`   // CIDRs or addresses
	Deny          []string                `json:"deny,omitempty"`
	DenyCountries []string                `json:"deny_countries,omitempty"`
	Routes        map[string]*RouteAccess `json:"routes,omitempty"`
}

// RouteAccess is a zone: a path prefix with rules added to the site's.
type RouteAccess struct {
	Password string   `json:"password,omitempty"` // the zone's own, asked after the site's
	Message  string   `json:"message,omitempty"`
	Allow    []string `json:"allow,omitempty"`
	Deny     []string `json:"deny,omitempty"`
	Note     string   `json:"note,omitempty"` // for whoever reads the manifest
}

// WAF is the per-site layer-7 policy.
type WAF struct {
	Rate        *Rate  `json:"rate,omitempty"`
	MaxBody     string `json:"max_body,omitempty"`
	UnderAttack bool   `json:"under_attack,omitempty"`
}

// Rate is a token bucket per client address.
type Rate struct {
	Requests int      `json:"requests"`
	Window   Duration `json:"window"`
	Burst    int      `json:"burst,omitempty"`
}

// Cache is what the house cache may keep.
type Cache struct {
	StaticMaxAge Duration `json:"static_max_age,omitempty"`
}

// Deploy is how new code gets in.
type Deploy struct {
	GitHub *GitHubDeploy `json:"github,omitempty"`
}

// GitHubDeploy deploys on push.
type GitHubDeploy struct {
	Repository string     `json:"repository"`
	Branch     string     `json:"branch,omitempty"`
	Build      [][]string `json:"build,omitempty"`
	Pointer    string     `json:"pointer,omitempty"`
}

// Agent chooses the conversation provider for this site.
type Agent struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// MonthlyCapUSD stops the conversation once the ledger passes it. Zero
	// means no cap.
	MonthlyCapUSD float64 `json:"monthly_cap_usd,omitempty"`
}

// Duration marshals as a Go duration string ("8s"), because "8000000000" in
// a file a person edits is a number nobody can read.
type Duration time.Duration

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts "8s" or a bare number of seconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration %s: want \"8s\" or seconds", b)
	}
	*d = Duration(time.Duration(n * float64(time.Second)))
	return nil
}

// Or returns d or fallback when d is zero.
func (d Duration) Or(fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}
	return time.Duration(d)
}

// Load reads and validates a site folder's manifest.
func Load(siteDir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(siteDir, FileName))
	if err != nil {
		return nil, err
	}
	m, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := m.CheckInside(siteDir); err != nil {
		return nil, err
	}
	return m, nil
}

// Parse decodes and validates manifest bytes. Unknown fields are an error:
// a typo in "comand" silently serving a folder as static files is exactly
// the kind of failure that looks like success.
func Parse(data []byte) (*Manifest, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Save writes m to the site folder atomically.
func Save(siteDir string, m *Manifest) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if err := m.CheckInside(siteDir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(siteDir, FileName), append(data, '\n'))
}

var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9-]{2,63}$`)

// ValidDomain reports whether s can name a site folder. Lower-case ASCII
// only: a folder is a route, and a route with a capital letter never matches
// a Host header, which arrives lower-cased.
func ValidDomain(s string) bool { return domainRe.MatchString(s) }

var projectRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Reserved names cannot be projects: "middleware" is the house's own
// conversation, and the rest are what a Host header without a dot says.
var reserved = map[string]bool{"middleware": true, "localhost": true}

// IsProject reports whether a site folder's name is a project: a name with
// no dot, which answers no hostname. A project has its workers, its tasks
// and its conversation, and nothing that serves a request.
func IsProject(s string) bool { return projectRe.MatchString(s) && !reserved[s] }

// ValidName reports whether s can name a folder under sites/: a domain or
// a project. What deals with hostnames (certificates, doors, aliases)
// asks ValidDomain instead.
func ValidName(s string) bool { return ValidDomain(s) || IsProject(s) }

// Validate checks the manifest is coherent. It rejects what the engine could
// not act on rather than what merely looks odd.
func (m *Manifest) Validate() error {
	if m.Version != Version {
		return fmt.Errorf("version: want %d, got %d", Version, m.Version)
	}
	if m.Run != nil {
		if len(m.Run.Command) == 0 {
			return errors.New("run.command: empty")
		}
		switch m.Run.Runtime {
		case "", "process", "docker":
		default:
			return fmt.Errorf("run.runtime: %q is not process or docker", m.Run.Runtime)
		}
		if m.Run.Runtime == "docker" && (m.Run.Docker == nil || (m.Run.Docker.Image == "" && m.Run.Docker.Build == "")) {
			return errors.New("run.docker: needs image or build")
		}
		if d := m.Run.Docker; d != nil {
			if d.Pids < 0 || d.CPUs < 0 || d.CPUWeight < 0 {
				return errors.New("run.docker: pids, cpus and cpu_weight cannot be negative")
			}
			if d.User != "" && !userRe.MatchString(d.User) {
				return fmt.Errorf("run.docker.user: %q is not uid[:gid]", d.User)
			}
		}
		for k := range m.Run.Env {
			if !validEnvKey(k) {
				return fmt.Errorf("run.env: %q is not a variable name", k)
			}
		}
		for k := range m.Run.EnvFromFile {
			if !validEnvKey(k) {
				return fmt.Errorf("run.env_from_file: %q is not a variable name", k)
			}
		}
		if m.Run.Port < 0 || m.Run.Port > 65535 {
			return fmt.Errorf("run.port: %d is not a port", m.Run.Port)
		}
	}
	for _, a := range m.Aliases {
		if !ValidDomain(a) {
			return fmt.Errorf("aliases: %q is not a domain", a)
		}
	}
	if m.Upstream != "" && !strings.HasPrefix(m.Upstream, "http://") && !strings.HasPrefix(m.Upstream, "https://") {
		return fmt.Errorf("upstream: %q must start with http:// or https://", m.Upstream)
	}
	for name, w := range m.Workers {
		if !validName(name) {
			return fmt.Errorf("workers: %q is not a name", name)
		}
		if len(w.Command) == 0 {
			return fmt.Errorf("workers.%s.command: empty", name)
		}
		if w.Port < 0 || w.Port > 65535 {
			return fmt.Errorf("workers.%s.port: %d is not a port", name, w.Port)
		}
		for k := range w.EnvFromFile {
			if !validEnvKey(k) {
				return fmt.Errorf("workers.%s.env_from_file: %q is not a variable name", name, k)
			}
		}
	}
	if m.WAF != nil && m.WAF.Rate != nil {
		if m.WAF.Rate.Requests <= 0 || m.WAF.Rate.Window <= 0 {
			return errors.New("waf.rate: requests and window must be positive")
		}
	}
	if m.Deploy != nil && m.Deploy.GitHub != nil && !strings.Contains(m.Deploy.GitHub.Repository, "/") {
		return errors.New("deploy.github.repository: want owner/name")
	}
	return nil
}

// Mode is what answers a request for this site, in the order docs/manifest.md
// lists.
type Mode string

// The modes.
const (
	ModeDisabled Mode = "disabled"
	ModeHold     Mode = "hold"
	ModeRun      Mode = "run"
	ModeUpstream Mode = "upstream"
	ModeStatic   Mode = "static"
	// ModeProject is a folder without a domain: workers, tasks and a
	// conversation, nothing that answers a request. The engine sets it
	// from the folder's name; a manifest cannot ask for it.
	ModeProject Mode = "project"
)

// Hosts is every name the site in the folder called name answers to and
// is published, certified and unpublished under: the folder's name, its
// aliases, and www. in front of an apex domain unless `www` is false.
// Everything that touches names asks this, so none of them can forget one:
// V1 published luma32.com and left www.luma32.com with no record and no
// certificate because nobody had written it as an alias. A project has no
// names at all.
func (m *Manifest) Hosts(name string) []string {
	if IsProject(name) || !ValidDomain(name) {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		h = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	add(name)
	for _, a := range m.Aliases {
		add(a)
	}
	if (m.WWW == nil || *m.WWW) && strings.Count(name, ".") == 1 && !strings.HasPrefix(name, "www.") {
		add("www." + name)
	}
	return out
}

// ModeFor is Mode for the folder called name: a project is a project
// unless it is disabled.
func (m *Manifest) ModeFor(name string) Mode {
	mode := m.Mode()
	if mode != ModeDisabled && IsProject(name) {
		return ModeProject
	}
	return mode
}

// Mode resolves the precedence rules.
func (m *Manifest) Mode() Mode {
	switch {
	case m.Disabled:
		return ModeDisabled
	case m.Hold != nil && m.Hold.Enabled:
		return ModeHold
	case m.Run != nil:
		return ModeRun
	case m.Upstream != "":
		return ModeUpstream
	default:
		return ModeStatic
	}
}

var userRe = regexp.MustCompile(`^[0-9]{1,10}(:[0-9]{1,10})?$`)
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func validEnvKey(s string) bool { return envKeyRe.MatchString(s) }
func validName(s string) bool   { return nameRe.MatchString(s) }
