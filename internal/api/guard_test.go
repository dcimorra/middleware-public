package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/guard"
)

func TestGuardListsRulesAndIncidentsAndDismissesOne(t *testing.T) {
	f := newFixture(t).withToken()
	dataDir := filepath.Join(f.root, "data")
	g := guard.New(guard.Options{ProcRoot: t.TempDir(), DataDir: dataDir, SitesDir: filepath.Join(f.root, "sites"), Bus: f.bus})
	f.srv.RegisterGuard(GuardOptions{Guard: g})
	os.MkdirAll(filepath.Join(dataDir, "guard", "incidents"), 0o755)
	inc := guard.Incident{ID: "inc-1", At: time.Now(), Domain: "x.test", Rule: "foreign-write", Level: guard.LevelSuspicious, Where: "/etc", Action: "logged"}
	if err := fsx.WriteJSON(filepath.Join(dataDir, "guard", "incidents", "inc-1.json"), inc, 0o644); err != nil {
		t.Fatal(err)
	}

	resp, body := f.call("GET", "/api/v1/guard", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get: %d %s", resp.StatusCode, text(body))
	}
	if rules := body["rules"].([]any); len(rules) == 0 {
		t.Fatal("no rules listed")
	}
	if incidents := body["incidents"].([]any); len(incidents) != 1 {
		t.Fatalf("incidents: %v", incidents)
	}
	resp, _ = f.call("POST", "/api/v1/guard/incidents/inc-1/dismiss", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("dismiss: %d", resp.StatusCode)
	}
	_, body = f.call("GET", "/api/v1/guard", nil)
	if incidents := body["incidents"].([]any); len(incidents) != 0 {
		t.Fatalf("dismissed incident still listed: %v", incidents)
	}
	resp, _ = f.call("POST", "/api/v1/guard/incidents/nope/dismiss", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown incident: %d", resp.StatusCode)
	}
	resp, _ = f.call("POST", "/api/v1/guard/incidents/..%2Fx/dismiss", nil)
	if resp.StatusCode == 200 {
		t.Fatal("a path in the id must be refused")
	}
}
