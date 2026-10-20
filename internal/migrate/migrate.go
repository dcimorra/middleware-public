// Package migrate turns a V1 site.json into a V2 one.
//
// It converts what V2 can do and refuses the rest: a V1 setting in use with
// no V2 equivalent yet is a blocker, and a site with a blocker is not
// converted at all. Converting it without the setting would publish a site
// that looks migrated and has quietly lost something (a VPN-only site served
// to everyone, a password zone gone), which is the failure that looks
// exactly like success. As V2 gains each feature, its blocker becomes a
// conversion here; docs/parity.md is the list.
//
// Settings that change nothing V1 would have done (an attack mode written
// but off, a health timeout V2 measures differently) are converted or
// dropped with a note, so the report says everything that changed.
package migrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/access"
	"github.com/dcimorra/middleware-public/internal/manifest"
)

// Result is one site's conversion.
type Result struct {
	Name string `json:"name"`
	// Manifest is the V2 manifest, nil when there are blockers.
	Manifest *manifest.Manifest `json:"manifest,omitempty"`
	// Blockers are V1 settings in use that V2 cannot honour yet.
	Blockers []string `json:"blockers,omitempty"`
	// Notes are changes worth knowing that lose nothing.
	Notes []string `json:"notes,omitempty"`
	// Skipped says why a folder is not a site to convert.
	Skipped string `json:"skipped,omitempty"`
}

// Ready reports whether the site can be converted.
func (r Result) Ready() bool { return r.Manifest != nil && len(r.Blockers) == 0 }

// v1 is the V1 manifest as far as this package needs it. Fields whose
// contents are never converted are kept raw, only to know they are there.
type v1 struct {
	Title       string            `json:"title"`
	Kind        string            `json:"kind"`
	Aliases     []string          `json:"aliases"`
	WWW         *bool             `json:"www"`
	Group       string            `json:"group"`
	App         string            `json:"app"`
	Disabled    bool              `json:"disabled"`
	AutoFix     *bool             `json:"autofix"`
	Hold        *v1Hold           `json:"hold"`
	Auth        *v1Auth           `json:"auth"`
	IP          *v1IP             `json:"ip"`
	VPN         json.RawMessage   `json:"vpn"`
	Security    json.RawMessage   `json:"security"`
	Attack      json.RawMessage   `json:"attack"`
	Notice      json.RawMessage   `json:"notice"`
	Root        string            `json:"root"`
	SPA         bool              `json:"spa"`
	Command     []string          `json:"command"`
	Dir         string            `json:"dir"`
	Env         map[string]string `json:"env"`
	EnvFiles    []string          `json:"env_files"`
	EnvFromFile map[string]string `json:"env_from_file"`
	Port        int               `json:"port"`
	Workers     []v1Worker        `json:"workers"`
	Routes      []v1Route         `json:"routes"`
	Cache       json.RawMessage   `json:"cache"`
	Needs       []string          `json:"needs"`
	Chat        json.RawMessage   `json:"chat"`
	Claude      bool              `json:"claude"`
	ClaudeAPI   json.RawMessage   `json:"claude_api"`
	Container   json.RawMessage   `json:"container"`
	Health      v1Health          `json:"health"`
}

type v1Hold struct {
	Mode    string `json:"mode"`
	Message string `json:"message"`
	Until   string `json:"until"`
}

type v1Auth struct {
	Password string `json:"password"`
	Hash     string `json:"hash"`
	Message  string `json:"message"`
}

type v1IP struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

// v1Attack and v1Cache are read leniently, only for whether they are on:
// their other fields are never converted.
type v1Attack struct {
	On   bool `json:"on"`
	Bits int  `json:"bits"`
}

type v1Cache struct {
	Enabled *bool             `json:"enabled"`
	Rules   []json.RawMessage `json:"rules"`
}

type v1Worker struct {
	Name        string            `json:"name"`
	Command     []string          `json:"command"`
	Dir         string            `json:"dir"`
	Env         map[string]string `json:"env"`
	EnvFiles    []string          `json:"env_files"`
	EnvFromFile map[string]string `json:"env_from_file"`
	Port        int               `json:"port"`
	Disabled    bool              `json:"disabled"`
	Note        string            `json:"note"`
}

type v1Route struct {
	Path string  `json:"path"`
	Note string  `json:"note"`
	Auth *v1Auth `json:"auth"`
	IP   *v1IP   `json:"ip"`
}

type v1Health struct {
	Path     string `json:"path"`
	Interval string `json:"interval"`
	Timeout  string `json:"timeout"`
	Expect   int    `json:"expect_status"`
}

// Convert converts the V1 manifest of the folder called name.
func Convert(name string, data []byte) Result {
	r := Result{Name: name}
	var in v1
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		r.Blockers = append(r.Blockers, "the V1 manifest does not read: "+err.Error())
		return r
	}
	c := converter{r: &r}
	m := &manifest.Manifest{Version: manifest.Version, Title: in.Title, Disabled: in.Disabled, WWW: in.WWW}
	c.blockers(&in)
	c.serve(&in, m, name)
	c.access(&in, m)
	c.workers(&in, m)
	c.hold(&in, m)
	if len(in.Aliases) > 0 {
		m.Aliases = append([]string(nil), in.Aliases...)
	}
	if len(r.Blockers) > 0 {
		return r
	}
	if err := m.Validate(); err != nil {
		r.Blockers = append(r.Blockers, "the converted manifest is not valid: "+err.Error())
		return r
	}
	r.Manifest = m
	return r
}

type converter struct{ r *Result }

func (c converter) block(format string, args ...any) {
	c.r.Blockers = append(c.r.Blockers, fmt.Sprintf(format, args...))
}

func (c converter) note(format string, args ...any) {
	c.r.Notes = append(c.r.Notes, fmt.Sprintf(format, args...))
}

func present(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != "{}" && s != "[]" && s != "false"
}

// blockers lists the V1 settings in use that V2 cannot honour yet, each
// with the phase of docs/parity.md that brings it.
func (c converter) blockers(in *v1) {
	if in.Group != "" {
		c.block("group %q (panel groups; parity §7)", in.Group)
	}
	if in.App != "" {
		c.block("app %q (apps; parity §8)", in.App)
	}
	if in.AutoFix != nil && *in.AutoFix {
		c.block("autofix (repair after failed health checks; parity §6)")
	}
	if present(in.VPN) {
		c.block("vpn (VPN-only site; parity §2)")
	}
	if present(in.Security) {
		c.block("security (bots, browsers, probe paths; parity §3)")
	}
	if present(in.Attack) {
		var a v1Attack
		_ = json.Unmarshal(in.Attack, &a)
		if a.On {
			c.block("attack mode on (proof of work; parity §3)")
		} else {
			c.note("attack: written but off; its settings are not carried (bits %d)", a.Bits)
		}
	}
	if present(in.Notice) {
		c.block("notice (holding page theme; parity §3)")
	}
	if present(in.Cache) {
		var cr v1Cache
		_ = json.Unmarshal(in.Cache, &cr)
		if cr.Enabled != nil && !*cr.Enabled {
			c.note("cache: written but off; its rules are not carried")
		} else if len(cr.Rules) > 0 {
			c.block("cache rules (house cache; parity §2)")
		}
	}
	if len(in.Needs) > 0 {
		c.block("needs %v (start order; parity §4)", in.Needs)
	}
	if present(in.Chat) {
		c.block("chat (loans to the conversation; parity §5)")
	}
	if in.Claude {
		c.block("claude: true (agent token for the site; parity §5)")
	}
	if present(in.ClaudeAPI) {
		c.block("claude_api (the site door; parity §5)")
	}
	if present(in.Container) {
		c.block("container (the site runs in docker; parity §4)")
	}
	if in.Health.Expect != 0 {
		c.block("health.expect_status %d (parity §4)", in.Health.Expect)
	}
}

// serve converts what answers a request.
func (c converter) serve(in *v1, m *manifest.Manifest, name string) {
	switch in.Kind {
	case "static", "":
		if len(in.Command) > 0 {
			c.block("kind static with a command: V1 never ran it, and V2 would not either")
		}
		if manifest.IsProject(name) {
			// V1 projects are written as static sites with workers.
			if in.Root != "" || in.SPA {
				c.note("root/spa dropped: a project serves nothing")
			}
			return
		}
		root := in.Root
		if root == "" {
			// V1 served public/ by default; V2 would serve the folder
			// itself when public/ is missing, sources included. Written
			// out so the two agree.
			root = "public"
		}
		m.Static = &manifest.Static{Root: root, SPA: in.SPA}
	case "proxy":
		if manifest.IsProject(name) {
			c.block("a project with a process of its own: make it a worker")
			return
		}
		if len(in.Command) == 0 {
			c.block("kind proxy without a command")
			return
		}
		run := &manifest.Run{
			Command:     in.Command,
			Cwd:         in.Dir,
			Env:         in.Env,
			EnvFiles:    in.EnvFiles,
			EnvFromFile: in.EnvFromFile,
			Port:        in.Port,
		}
		h := &manifest.Health{Path: in.Health.Path}
		if d, ok := c.duration("health.interval", in.Health.Interval); ok {
			h.Interval = manifest.Duration(d)
		}
		if in.Health.Timeout != "" {
			// V1's timeout is per check; V2's is how long a new
			// generation has to get ready, and a check is bounded on its
			// own. Carrying "8s" over would fail every cold start.
			c.note("health.timeout %s dropped: in V2 it is the start timeout (default 60s)", in.Health.Timeout)
		}
		if h.Path != "" || h.Interval != 0 {
			run.Health = h
		}
		if in.Root != "" {
			c.note("root %q dropped: a proxy site serves its process, not files", in.Root)
		}
		m.Run = run
	default:
		c.block("kind %q is not static or proxy", in.Kind)
	}
}

func (c converter) duration(field, s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		c.block("%s %q is not a duration", field, s)
		return 0, false
	}
	return d, true
}

// password turns a V1 auth into a V2 hash. The algorithm is the same
// (PBKDF2-SHA256, raw base64), only the framing differs; a plain password
// is hashed so it stops being written in the clear.
func (c converter) password(where string, a *v1Auth) string {
	if a == nil {
		return ""
	}
	if a.Hash != "" {
		parts := strings.Split(a.Hash, "$")
		if len(parts) != 4 || parts[0] != "pbkdf2" {
			c.block("%s: hash in an unknown format", where)
			return ""
		}
		return "$pbkdf2-sha256$" + parts[1] + "$" + parts[2] + "$" + parts[3]
	}
	if a.Password != "" {
		h, err := access.HashPassword(a.Password)
		if err != nil {
			c.block("%s: %v", where, err)
			return ""
		}
		c.note("%s: the password was written in the clear; it is stored hashed now", where)
		return h
	}
	return ""
}

func (c converter) access(in *v1, m *manifest.Manifest) {
	ac := &manifest.Access{}
	if in.Auth != nil {
		ac.Password = c.password("auth", in.Auth)
		ac.Message = in.Auth.Message
	}
	if in.IP != nil {
		ac.Allow, ac.Deny = in.IP.Allow, in.IP.Deny
	}
	for _, rt := range in.Routes {
		if rt.Path == "" || !strings.HasPrefix(rt.Path, "/") {
			c.block("routes: %q is not a path", rt.Path)
			continue
		}
		ra := &manifest.RouteAccess{Note: rt.Note}
		if rt.Auth != nil {
			ra.Password = c.password("routes "+rt.Path, rt.Auth)
			ra.Message = rt.Auth.Message
		}
		if rt.IP != nil {
			ra.Allow, ra.Deny = rt.IP.Allow, rt.IP.Deny
		}
		if ac.Routes == nil {
			ac.Routes = map[string]*manifest.RouteAccess{}
		}
		if _, dup := ac.Routes[rt.Path]; dup {
			c.block("routes: %s written twice", rt.Path)
		}
		ac.Routes[rt.Path] = ra
	}
	if ac.Password != "" || len(ac.Allow) > 0 || len(ac.Deny) > 0 || len(ac.Routes) > 0 {
		m.Access = ac
	}
}

func (c converter) workers(in *v1, m *manifest.Manifest) {
	for _, w := range in.Workers {
		if m.Workers == nil {
			m.Workers = map[string]*manifest.Worker{}
		}
		if _, dup := m.Workers[w.Name]; dup {
			c.block("workers: %q written twice", w.Name)
			continue
		}
		m.Workers[w.Name] = &manifest.Worker{
			Command: w.Command, Cwd: w.Dir, Port: w.Port, Env: w.Env,
			EnvFiles: w.EnvFiles, EnvFromFile: w.EnvFromFile,
			Disabled: w.Disabled, Note: w.Note,
		}
	}
}

func (c converter) hold(in *v1, m *manifest.Manifest) {
	if in.Hold == nil {
		return
	}
	switch {
	case in.Hold.Until != "":
		c.block("hold.until (parity §3)")
	case in.Hold.Mode != "" && in.Hold.Mode != "mantenimiento":
		c.block("hold mode %q (404/410/500 pages; parity §3)", in.Hold.Mode)
	default:
		m.Hold = &manifest.Hold{Enabled: true, Message: in.Hold.Message}
	}
}

// Summary counts blockers across results, most common first: the order in
// which closing them migrates the most sites.
func Summary(results []Result) []string {
	count := map[string]int{}
	for _, r := range results {
		seen := map[string]bool{}
		for _, b := range r.Blockers {
			key := b
			if i := strings.IndexAny(b, " :("); i > 0 {
				key = b[:i]
			}
			if !seen[key] {
				seen[key] = true
				count[key]++
			}
		}
	}
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if count[keys[i]] != count[keys[j]] {
			return count[keys[i]] > count[keys[j]]
		}
		return keys[i] < keys[j]
	})
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = fmt.Sprintf("%-14s %d site(s)", k, count[k])
	}
	return out
}
