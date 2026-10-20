// Package settings is data/settings.json: what this installation decided
// that is not about one site. It is small on purpose. Anything about a site
// goes in its manifest; anything about a module goes in the module store.
package settings

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dcimorra/middleware-public/internal/fsx"
)

// Settings is the file.
type Settings struct {
	// PanelDomain answers the panel. Empty means loopback only.
	PanelDomain string `json:"panel_domain,omitempty"`
	// Listen is the loopback address the tunnel door pushes to.
	Listen string `json:"listen,omitempty"`
	// Ports is the site port range.
	Ports struct {
		From int `json:"from,omitempty"`
		To   int `json:"to,omitempty"`
	} `json:"ports"`
	// VisitorTheme is the design of the visitor pages that belong to no
	// site, and of sites that did not choose one (see internal/pages).
	// It replaces a "pages" text override that nothing ever read.
	VisitorTheme string `json:"visitor_theme,omitempty"`
	// Agents caps how many conversations run at once.
	Agents Agents `json:"agents"`
	// Deps is how the external CLIs are kept.
	Deps Deps `json:"deps"`
	// Autofix hands a site that went down to its agent (internal/autofix).
	Autofix Autofix `json:"autofix"`
}

// Autofix is the automatic repair's switch and brakes. Off by default:
// it acts while the operator sleeps, so the brakes are the important part.
type Autofix struct {
	Enabled bool `json:"enabled,omitempty"`
	// Attempts per fault, then it stops and leaves it to the operator.
	Attempts int `json:"attempts,omitempty"`
	// WaitMinutes between attempts on one site.
	WaitMinutes int `json:"wait_minutes,omitempty"`
	// DailyUSD caps what all repairs together may spend in a day.
	DailyUSD float64 `json:"daily_usd,omitempty"`
}

// Limits fills the defaults: 2 attempts, 30 minutes, 2 USD a day.
func (a Autofix) Limits() Autofix {
	if a.Attempts <= 0 {
		a.Attempts = 2
	}
	if a.WaitMinutes <= 0 {
		a.WaitMinutes = 30
	}
	if a.DailyUSD <= 0 {
		a.DailyUSD = 2
	}
	return a
}

// Deps is the section about the programs the modules run (the agent
// CLIs). They come from npm and have release channels of their own, so
// the installation checks for a newer one daily and, by default,
// installs it: a CLI a year behind is a chat that stops working one day
// with nobody having changed anything here.
type Deps struct {
	// AutoUpdate installs a newer release when the daily check finds
	// one. Off, the check only reports it in the panel.
	AutoUpdate *bool `json:"auto_update,omitempty"`
}

// AutoUpdateOn reports the setting with its default (on).
func (d Deps) AutoUpdateOn() bool { return d.AutoUpdate == nil || *d.AutoUpdate }

// Agents is the concurrency section. Every agent turn is a CLI process
// with a model behind it, and a machine that serves forty sites cannot
// run forty of them at once; the cap is house-wide, with a few slots kept
// for the installation's own conversation so the operator can still ask
// the house what is going on when every site is busy.
type Agents struct {
	// MaxConcurrent is the number of turns running at once, all
	// conversations together. 1..64.
	MaxConcurrent int `json:"max_concurrent"`
	// ReservedForHouse is how many of those only the middleware
	// conversation may take. Less than MaxConcurrent.
	ReservedForHouse int `json:"reserved_for_house"`
	// PerProvider caps one provider's share; 0 means no cap of its own.
	PerProvider map[string]int `json:"per_provider"`
	// DefaultProvider runs a conversation whose manifest names none.
	DefaultProvider string `json:"default_provider"`
	// SiteDailyUSD is what a site may spend through the site door in a
	// day when its manifest says nothing (claude_api.daily_usd).
	SiteDailyUSD float64 `json:"site_daily_usd,omitempty"`
}

// SiteDaily is SiteDailyUSD with its default.
func (a Agents) SiteDaily() float64 {
	if a.SiteDailyUSD > 0 {
		return a.SiteDailyUSD
	}
	return 5
}

// MaxAgents is the ceiling of MaxConcurrent.
const MaxAgents = 64

// DefaultAgents is the section a fresh installation runs with.
func DefaultAgents() Agents {
	return Agents{MaxConcurrent: 8, ReservedForHouse: 1, PerProvider: map[string]int{"claude": 0, "gemini": 0}, DefaultProvider: "claude"}
}

// Validate says what is wrong with the section, if anything.
func (a Agents) Validate() error {
	if a.MaxConcurrent < 1 || a.MaxConcurrent > MaxAgents {
		return fmt.Errorf("agents.max_concurrent: want 1..%d", MaxAgents)
	}
	if a.ReservedForHouse < 0 || a.ReservedForHouse >= a.MaxConcurrent {
		return errors.New("agents.reserved_for_house: want 0 <= reserved < max_concurrent")
	}
	for k, v := range a.PerProvider {
		if v < 0 || v > MaxAgents {
			return fmt.Errorf("agents.per_provider.%s: want 0..%d", k, MaxAgents)
		}
	}
	if a.DefaultProvider == "" {
		return errors.New("agents.default_provider: empty")
	}
	return nil
}

// Defaults are what a fresh installation runs with.
func Defaults() Settings {
	var s Settings
	s.Listen = "127.0.0.1:46100"
	s.Ports.From, s.Ports.To = 33000, 47000
	s.Agents = DefaultAgents()
	return s
}

// Path is where the file lives under a root.
func Path(root string) string { return filepath.Join(root, "data", "settings.json") }

// Load reads the file, returning defaults when it does not exist yet.
func Load(root string) (Settings, error) {
	s := Defaults()
	err := fsx.ReadJSON(Path(root), &s)
	if err != nil && !os.IsNotExist(err) {
		return s, err
	}
	s.Fill()
	return s, nil
}

// Fill puts the defaults where a field was left empty: a file from before
// a section existed, or a PUT from a panel that does not know it yet,
// must not turn into "no agents may run".
func (s *Settings) Fill() {
	if s.Listen == "" {
		s.Listen = "127.0.0.1:46100"
	}
	if s.Ports.From == 0 || s.Ports.To == 0 {
		s.Ports.From, s.Ports.To = 33000, 47000
	}
	d := DefaultAgents()
	if s.Agents.MaxConcurrent == 0 && s.Agents.DefaultProvider == "" && s.Agents.PerProvider == nil {
		s.Agents = d
		return
	}
	if s.Agents.DefaultProvider == "" {
		s.Agents.DefaultProvider = d.DefaultProvider
	}
	if s.Agents.PerProvider == nil {
		s.Agents.PerProvider = d.PerProvider
	}
}

// Save writes the file atomically.
func Save(root string, s Settings) error { return fsx.WriteJSON(Path(root), s, 0o644) }

// Root resolves the installation root: the flag, then MIDDLEWARE_ROOT, then
// the current directory if it already holds a sites/ folder, then the
// system default.
func Root(flag string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv("MIDDLEWARE_ROOT"); v != "" {
		return v
	}
	if wd, err := os.Getwd(); err == nil && fsx.Exists(filepath.Join(wd, "sites")) {
		return wd
	}
	return "/srv/middleware"
}
