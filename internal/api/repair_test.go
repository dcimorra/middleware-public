package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/agent"
	"github.com/dcimorra/middleware-public/internal/chat"
	"github.com/dcimorra/middleware-public/internal/settings"
)

// An arrived site that does not start is handed to the house's AI for
// automatic work, with the provider and model chosen for it, and the job
// says the AI's closing line; with none configured the target says so and
// the move is undone as before.
func TestAnArrivedSiteIsRepairedByTheHousesAIForAutomaticWork(t *testing.T) {
	f := newFixture(t).withToken()
	release := make(chan struct{})
	close(release)
	reg := agent.NewRegistry()
	reg.Register(holdingProvider{key: "claude", release: release})
	m := chat.New(chat.Options{Root: f.root, Bus: f.bus, Registry: reg})
	f.srv.RegisterRepair(RepairOptions{Chat: m})
	os.MkdirAll(filepath.Join(f.root, "sites", "r.test"), 0o755)
	os.WriteFile(filepath.Join(f.root, "sites", "r.test", "site.json"), []byte(`{"version":2,"run":{"command":["node","server.js"]}}`), 0o644)

	if res, out := f.call("POST", "/api/v1/relocate/r.test/repair", map[string]string{"why": "not ready"}); res.StatusCode != 409 || !strings.Contains(text(out), "no AI for automatic work") {
		t.Fatalf("without one: %d %s", res.StatusCode, text(out))
	}
	st := settings.Defaults()
	st.Automation = settings.Automation{Provider: "claude", Model: "opus"}
	f.srv.opts.Settings = func() settings.Settings { return st }
	if res, out := f.call("POST", "/api/v1/relocate/r.test/repair", map[string]string{"why": "Cannot find module '@prisma/client-2c3a'"}); res.StatusCode != 200 {
		t.Fatalf("start: %d %s", res.StatusCode, text(out))
	}
	deadline := time.Now().Add(10 * time.Second)
	var job map[string]any
	for time.Now().Before(deadline) {
		_, job = f.call("GET", "/api/v1/relocate/r.test/repair", nil)
		if job["done"] == true {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if job["done"] != true || job["error"] != nil || job["output"] != "ok" {
		t.Fatalf("job: %v", job)
	}
	c, err := m.History("r.test")
	if err != nil || len(c.Turns) == 0 || !strings.Contains(c.Turns[0].Text, "@prisma/client-2c3a") {
		t.Fatalf("the AI was not told why: %+v %v", c, err)
	}
	if last := c.Turns[len(c.Turns)-1]; last.Provider != "claude" {
		t.Fatalf("answered by %q", last.Provider)
	}
}
