package sites

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Stopping or holding one service is a decision written to the manifest,
// as a site's: it survives a restart, and nothing else in it changes.
func TestStoppingOrHoldingAServiceIsWrittenToItsSection(t *testing.T) {
	f := newFixture(t, false)
	dir := filepath.Join(f.root, "sites", "bakery.test")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"services":{
		"web":{"run":{"command":["node","web.js"]},"domains":["bakery.test"]},
		"api":{"run":{"command":["node","api.js"]},"domains":["api.bakery.test"]}}}`), 0o644)
	if _, err := f.m.SetServiceDisabled("bakery.test", "api", true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.SetServiceHold("bakery.test", "web", manifest.Hold{Enabled: true, Title: "Back soon"}); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Services["api"].Disabled || m.Disabled || m.Services["web"].Disabled {
		t.Errorf("disabled: site %v web %v api %v", m.Disabled, m.Services["web"].Disabled, m.Services["api"].Disabled)
	}
	if h := m.Services["web"].Hold; h == nil || !h.Enabled || h.Title != "Back soon" || m.Hold != nil {
		t.Errorf("hold: %+v, site's %+v", h, m.Hold)
	}
	if _, err := f.m.SetServiceDisabled("bakery.test", "db", true); err == nil {
		t.Error("a service the site does not have")
	}
	f.m.SetServiceDisabled("bakery.test", "api", false)
	f.m.SetServiceHold("bakery.test", "web", manifest.Hold{})
	m, _ = manifest.Load(dir)
	if m.Services["api"].Disabled || m.Services["web"].Hold != nil {
		t.Error("starting and unholding clear what stopping and holding wrote")
	}
}
