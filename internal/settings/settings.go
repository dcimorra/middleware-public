// Package settings is data/settings.json: what this installation decided
// that is not about one site. It is small on purpose. Anything about a site
// goes in its manifest; anything about a module goes in the module store.
package settings

import (
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
	// Pages overrides visitor page texts: key → [title, message].
	Pages map[string][2]string `json:"pages,omitempty"`
}

// Defaults are what a fresh installation runs with.
func Defaults() Settings {
	var s Settings
	s.Listen = "127.0.0.1:46100"
	s.Ports.From, s.Ports.To = 33000, 47000
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
	if s.Listen == "" {
		s.Listen = "127.0.0.1:46100"
	}
	if s.Ports.From == 0 || s.Ports.To == 0 {
		s.Ports.From, s.Ports.To = 33000, 47000
	}
	return s, nil
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
