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
	Version  int                `json:"version"`
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
	Command     []string          `json:"command"`
	Cwd         string            `json:"cwd,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	EnvFile     string            `json:"env_file,omitempty"`
	EnvFromFile map[string]string `json:"env_from_file,omitempty"`
	Health      *Health           `json:"health,omitempty"`
	Drain       Duration          `json:"drain,omitempty"`
	Restart     *Restart          `json:"restart,omitempty"`
	Runtime     string            `json:"runtime,omitempty"` // "process" (default) | "docker"
	Docker      *Docker           `json:"docker,omitempty"`
}

// Health is how the engine knows a new instance is ready.
type Health struct {
	Path    string   `json:"path,omitempty"`
	Timeout Duration `json:"timeout,omitempty"`
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
}

// Worker is an always-on process with a fixed port and no domain.
type Worker struct {
	Command []string          `json:"command"`
	Cwd     string            `json:"cwd,omitempty"`
	Port    int               `json:"port"`
	Env     map[string]string `json:"env,omitempty"`
	EnvFile string            `json:"env_file,omitempty"`
}

// Hold shows a page instead of the site, with the process stopped.
type Hold struct {
	Enabled bool   `json:"enabled"`
	Title   string `json:"title,omitempty"`
	Message string `json:"message,omitempty"`
}

// Access decides who gets in.
type Access struct {
	Password      string                  `json:"password,omitempty"` // hashed, see internal/auth
	Allow         []string                `json:"allow,omitempty"`    // CIDRs
	DenyCountries []string                `json:"deny_countries,omitempty"`
	Routes        map[string]*RouteAccess `json:"routes,omitempty"`
}

// RouteAccess narrows Access for a path prefix.
type RouteAccess struct {
	Allow    []string `json:"allow,omitempty"`
	Password bool     `json:"password,omitempty"`
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
	return Parse(data)
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
		if w.Port <= 0 || w.Port > 65535 {
			return fmt.Errorf("workers.%s.port: a worker's port is written, got %d", name, w.Port)
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
)

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

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func validEnvKey(s string) bool { return envKeyRe.MatchString(s) }
func validName(s string) bool   { return nameRe.MatchString(s) }
