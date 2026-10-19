// Package modules is the registry of what not every installation wants:
// Cloudflare, GitHub deploys, agents, the VPN. A module declares what it
// needs, how to check that what it was given actually works, and what it
// installs. The one rule here is verify-then-save: credentials reach the
// store only after every permission has been probed and none failed. A
// store that holds a token that was never checked is a panel that says
// "enabled" over a module that silently does nothing.
package modules

import (
	"context"
	"net/http"
	"regexp"
)

// Need is one value a module asks the operator for.
type Need struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Secret bool   `json:"secret"`
	// Help says where to get the value and which permissions it needs;
	// it is shown next to the field.
	Help string `json:"help,omitempty"`
	// Optional needs may be empty: the module discovers or generates them.
	Optional bool `json:"optional,omitempty"`
}

// Config is what the operator typed, by need key, in clear.
type Config map[string]string

// Line is one result of a verification: one probe, one sentence.
type Line struct {
	Status string `json:"status"` // StatusOK | StatusSkip | StatusFail
	Text   string `json:"text"`
}

// The three statuses a Line can carry. Skip is for what cannot be probed
// without side effects: it is said explicitly, never reported as ok.
const (
	StatusOK   = "ok"
	StatusSkip = "skip"
	StatusFail = "fail"
)

// OK builds an ok line.
func OK(text string) Line { return Line{Status: StatusOK, Text: text} }

// Skip builds a skip line.
func Skip(text string) Line { return Line{Status: StatusSkip, Text: text} }

// Fail builds a fail line.
func Fail(text string) Line { return Line{Status: StatusFail, Text: text} }

// Failed reports whether any line failed.
func Failed(lines []Line) bool {
	for _, l := range lines {
		if l.Status == StatusFail {
			return true
		}
	}
	return false
}

// Module is what a package under internal/modules implements.
type Module interface {
	// Key names the module in the store, the API and the CLI: ^[a-z0-9-]+$.
	Key() string
	Name() string
	Description() string
	// Needs lists the values the module asks for.
	Needs() []Need
	// Verify probes every permission it can without creating anything real
	// and reports one line per probe. It must not write to disk or to the
	// service; it may fill values it discovers into cfg (an account id),
	// and on Enable that same map reaches Install and is saved.
	Verify(ctx context.Context, cfg Config) []Line
	// Install fetches what the module needs on disk (a binary, a config
	// file). progress receives one line per step for the panel and the CLI.
	Install(ctx context.Context, cfg Config, progress func(string)) error
	// Enable runs after a successful verify and install, with the config
	// that is about to be saved. Values the module fills in (a generated
	// webhook secret, a discovered account id) are written into cfg and
	// saved with it.
	Enable(ctx context.Context, cfg Config) error
	// Disable stops whatever Enable started. Credentials stay in the store.
	Disable(ctx context.Context) error
	// Routes are mounted by the API under /api/v1/modules/<key>/; the key
	// is the path below that prefix ("hook" → /api/v1/modules/github/hook).
	// May be nil.
	Routes() map[string]http.Handler
}

// Binaries is implemented by modules that install executables under
// modules/bin, so Manager.Installed can answer without asking the module
// to run anything.
type Binaries interface {
	Binaries() []string
}

// Info is a module as the panel and the CLI list it.
type Info struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	Installed   bool   `json:"installed"`
	// Configured says the store holds a config for it, enabled or not: a
	// disabled module with credentials kept is a different thing from one
	// never set up, and the panel shows them differently.
	Configured bool   `json:"configured"`
	Needs      []Need `json:"needs"`
}

var keyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidKey reports whether s can be a module key.
func ValidKey(s string) bool { return keyRe.MatchString(s) }
