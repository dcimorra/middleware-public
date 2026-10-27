package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/relocate"
	"github.com/dcimorra/middleware-public/internal/tasks"
)

// A V1 site comes into a V2 house with the token the V2 panel made for it:
// converted, put in place, and stopped in V1. The token opens that site's
// import and nothing else.
func TestAV1SiteIsImportedWithItsOneSiteToken(t *testing.T) {
	v2, _ := inboundFixture(t, &fakeDNS{enabled: true})
	_, out := v2.call("POST", "/api/v1/relocate/import-token", map[string]any{"domain": "old.test", "base": v2.http.URL})
	tok, _ := out["token"].(string)
	if !strings.HasPrefix(tok, "mw_") || !strings.Contains(out["command"].(string), "import --to") {
		t.Fatalf("import token: %v", out)
	}

	v1 := t.TempDir()
	site := filepath.Join(v1, "sites", "old.test")
	os.MkdirAll(filepath.Join(site, "public"), 0o755)
	os.WriteFile(filepath.Join(site, "public", "index.html"), []byte("from v1"), 0o644)
	os.WriteFile(filepath.Join(site, "package.json"), []byte(`{"name":"old"}`), 0o644)
	os.WriteFile(filepath.Join(site, "package-lock.json"), []byte(`{"name":"old","lockfileVersion":3,"requires":true,"packages":{"":{"name":"old"}}}`), 0o644)
	os.MkdirAll(filepath.Join(site, "node_modules", "x"), 0o755)
	os.WriteFile(filepath.Join(site, "node_modules", "x", "big.js"), []byte("built for the other machine"), 0o644)
	os.WriteFile(filepath.Join(site, "site.json"), []byte(`{"title":"old","root":"public","aliases":["www2.old.test"]}`), 0o644)

	src := &relocate.V1Source{Root: v1, Running: func(string) bool { return false }}
	if _, err := src.Convert("old.test"); err != nil {
		t.Fatal(err)
	}
	m := &relocate.Mover{Root: v1, Source: src, Parts: relocate.ImportParts, Poll: 20 * time.Millisecond, ReadyWait: 5 * time.Second}
	target := relocate.HTTPTarget{Base: v2.http.URL, Token: tok, Install: true}
	if _, err := m.Move(context.Background(), "old.test", target, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(v2.root, "sites", "old.test", "public", "index.html")); string(got) != "from v1" {
		t.Fatalf("index: %q", got)
	}
	if _, err := os.Stat(filepath.Join(v2.root, "sites", "old.test", "node_modules")); err == nil {
		t.Fatal("node_modules was carried: it is installed on the target, for its machine")
	}
	var mf map[string]any
	raw, _ := os.ReadFile(filepath.Join(v2.root, "sites", "old.test", "site.json"))
	json.Unmarshal(raw, &mf)
	if mf["version"] != float64(2) || mf["disabled"] == true {
		t.Fatalf("the manifest arrived as %s", raw)
	}
	// Stopped in V1, the way V1 stops a site, the rest of it untouched.
	raw, _ = os.ReadFile(filepath.Join(site, "site.json"))
	if !strings.Contains(string(raw), `"disabled": true`) || !strings.Contains(string(raw), `"root": "public"`) {
		t.Fatalf("V1 manifest: %s", raw)
	}

	// The token opens that site's import, not the house.
	scoped := func(method, path string) int {
		req, _ := http.NewRequest(method, v2.http.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if c := scoped("GET", "/api/v1/sites"); c != 403 {
		t.Fatalf("listing the sites with an import token: %d", c)
	}
	if c := scoped("PUT", "/api/v1/relocate/other.test"); c != 403 {
		t.Fatalf("another site's import with this token: %d", c)
	}
	if c := scoped("POST", "/api/v1/relocate/import-token"); c != 403 {
		t.Fatalf("an import token making tokens: %d", c)
	}
}

func TestAnImportTokenStopsWorkingAfterItsTwoHours(t *testing.T) {
	f := newFixture(t).withToken()
	pub, secret, err := f.store.CreateExpiringToken("import x.test", []string{"import:x.test"}, time.Millisecond)
	if err != nil || pub.Expires.IsZero() {
		t.Fatalf("%+v %v", pub, err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, ok := f.store.LookupToken(secret); ok {
		t.Fatal("an expired token still opens")
	}
}

func TestAV1SiteVTwoCannotHonourIsRefusedBeforeAnythingMoves(t *testing.T) {
	v1 := t.TempDir()
	site := filepath.Join(v1, "sites", "odd.test")
	os.MkdirAll(site, 0o755)
	os.WriteFile(filepath.Join(site, "site.json"), []byte(`{"root":"public","something_v2_does_not_know":1}`), 0o644)
	if _, err := (&relocate.V1Source{Root: v1}).Convert("odd.test"); err == nil {
		t.Fatal("a manifest the converter cannot read was accepted")
	}
}

// A node with no panel domain of its own cannot be reached from the V1
// machine: the import token says so instead of printing its mesh address.
func TestAnImportTokenNeverPointsAtTheRequestsHost(t *testing.T) {
	f := newFixture(t).withToken()
	resp, out := f.call("POST", "/api/v1/relocate/import-token", map[string]any{"domain": "x.test"})
	if resp.StatusCode != 400 || !strings.Contains(out["error"].(string), "panel domain") {
		t.Fatalf("no base and no panel domain: %d %v", resp.StatusCode, out)
	}
	_, out = f.call("POST", "/api/v1/relocate/import-token", map[string]any{"domain": "x.test", "base": "panel.example.com"})
	if !strings.Contains(out["command"].(string), "--to https://panel.example.com ") {
		t.Fatalf("command: %v", out["command"])
	}
}

// Any app folder from any server comes in the same way: its manifest
// worked out from what it holds, stopped and started with the commands the
// operator gives.
func TestAnAppFromAnyServerIsImportedWithItsOwnStopAndStart(t *testing.T) {
	v2, _ := inboundFixture(t, &fakeDNS{enabled: true})
	_, out := v2.call("POST", "/api/v1/relocate/import-token", map[string]any{"domain": "shop.test", "base": v2.http.URL})
	tok, _ := out["token"].(string)
	app := filepath.Join(t.TempDir(), "var", "www", "shop")
	os.MkdirAll(app, 0o755)
	os.WriteFile(filepath.Join(app, "index.html"), []byte("the shop"), 0o644)
	marks := t.TempDir()
	src := &relocate.GenericSource{Dir: app, StopCmd: "touch " + filepath.Join(marks, "stopped"), StartCmd: "touch " + filepath.Join(marks, "started")}
	if _, err := src.Detect("shop.test"); err != nil {
		t.Fatal(err)
	}
	m := &relocate.Mover{Root: filepath.Dir(app), Dir: app, Source: src, Parts: src.Parts, Install: true, Poll: 20 * time.Millisecond, ReadyWait: 5 * time.Second}
	var steps []string
	if _, err := m.Move(context.Background(), "shop.test", relocate.HTTPTarget{Base: v2.http.URL, Token: tok, Install: true}, func(step, msg string) { steps = append(steps, step+": "+msg) }); err != nil {
		t.Fatal(err)
	}
	// The install ran on the house's own, through the import token, and
	// said what it did.
	if got := strings.Join(steps, "\n"); !strings.Contains(got, "install: no package.json") {
		t.Fatalf("steps:\n%s", got)
	}
	if got, _ := os.ReadFile(filepath.Join(v2.root, "sites", "shop.test", "index.html")); string(got) != "the shop" {
		t.Fatalf("index: %q", got)
	}
	if _, err := os.Stat(filepath.Join(marks, "stopped")); err != nil {
		t.Fatal("its stop command was not run")
	}
	if _, err := os.Stat(filepath.Join(marks, "started")); err == nil {
		t.Fatal("it was started again although the import worked")
	}
	if _, err := os.Stat(filepath.Join(app, "index.html")); err != nil {
		t.Fatal("the app's folder was touched where it was")
	}
}

// An imported site's names follow it, as a move's do: once it answers
// here they are published over what pointed at the old server, with the
// import token, and nobody presses "Point it here". The token opens that
// for its own site only.
func TestAnImportedSitesNamesArePointedHereByThemselves(t *testing.T) {
	dns := &fakeDNS{enabled: true}
	v2, _ := inboundFixture(t, dns)
	_, out := v2.call("POST", "/api/v1/relocate/import-token", map[string]any{"domain": "shop.test", "base": v2.http.URL})
	tok, _ := out["token"].(string)
	app := filepath.Join(t.TempDir(), "shop")
	os.MkdirAll(app, 0o755)
	os.WriteFile(filepath.Join(app, "index.html"), []byte("the shop"), 0o644)
	src := &relocate.GenericSource{Dir: app}
	if _, err := src.Detect("shop.test"); err != nil {
		t.Fatal(err)
	}
	m := &relocate.Mover{Root: filepath.Dir(app), Dir: app, Source: src, Parts: src.Parts, Poll: 20 * time.Millisecond, ReadyWait: 5 * time.Second}
	rep, err := m.Move(context.Background(), "shop.test", relocate.HTTPTarget{Base: v2.http.URL, Token: tok}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.DNS != "moved" {
		t.Fatalf("names: %q", rep.DNS)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		pub, _ := dns.calls()
		if strings.Contains(strings.Join(pub, ","), "shop.test") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("published: %v", pub)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Another site's import token cannot take names over for this one.
	_, out = v2.call("POST", "/api/v1/relocate/import-token", map[string]any{"domain": "other.test", "base": v2.http.URL})
	other, _ := out["token"].(string)
	req, _ := http.NewRequest("POST", v2.http.URL+"/api/v1/sites/shop.test/publish", strings.NewReader(`{"over":true}`))
	req.Header.Set("Authorization", "Bearer "+other)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatalf("another site's token publishing over: %d", res.StatusCode)
	}
}

// A V1 site's tasks go with it, through the commit's body: converted,
// running there, and turned off in V1, where they would otherwise run
// against the same database the moment someone starts the site again.
func TestAV1SitesTasksGoWithItAndAreTurnedOffInV1(t *testing.T) {
	v2, _ := inboundFixture(t, &fakeDNS{enabled: true})
	_, out := v2.call("POST", "/api/v1/relocate/import-token", map[string]any{"domain": "old.test", "base": v2.http.URL})
	tok, _ := out["token"].(string)

	v1 := t.TempDir()
	site := filepath.Join(v1, "sites", "old.test")
	os.MkdirAll(filepath.Join(site, "public"), 0o755)
	os.MkdirAll(filepath.Join(site, "node_modules", ".bin"), 0o755)
	os.WriteFile(filepath.Join(site, "node_modules", ".bin", "tsx"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(site, "site.json"), []byte(`{"title":"old","root":"public"}`), 0o644)
	os.MkdirAll(filepath.Join(v1, "tasks"), 0o755)
	nightly := `{"title":"Sync","group":"old","note":"why at 00:50","schedule":"50 0 * * *","command":["tsx","scripts/sync.ts"],"site":"old.test","env":{"NODE_ENV":"production"},"timezone":"Europe/Madrid","timeout":"4h"}`
	off := `{"title":"Old","group":"old","schedule":"0 5 * * *","command":["/usr/bin/bash","x.sh"],"site":"old.test","disabled":true}`
	other := `{"title":"Other","group":"x","schedule":"0 5 * * *","command":["/bin/true"],"site":"other.test"}`
	for name, body := range map[string]string{"old-sync": nightly, "old-off": off, "other-task": other} {
		os.WriteFile(filepath.Join(v1, "tasks", name+".json"), []byte(body), 0o644)
	}

	src := &relocate.V1Source{Root: v1, Running: func(string) bool { return false }}
	if _, err := src.Convert("old.test"); err != nil {
		t.Fatal(err)
	}
	m := &relocate.Mover{Root: v1, Source: src, Parts: relocate.ImportParts, Poll: 20 * time.Millisecond, ReadyWait: 5 * time.Second}
	if _, err := m.Move(context.Background(), "old.test", relocate.HTTPTarget{Base: v2.http.URL, Token: tok}, nil); err != nil {
		t.Fatal(err)
	}

	got, err := tasks.Load(v2.root, "old-sync")
	if err != nil {
		t.Fatal(err)
	}
	if got.Timezone != "Europe/Madrid" || got.Title != "Sync" || got.Note != "why at 00:50" || time.Duration(got.Timeout) != 4*time.Hour ||
		got.Command[0] != "./node_modules/.bin/tsx" || got.Site != "old.test" || got.Disabled {
		t.Fatalf("the task arrived as %+v", got)
	}
	if g, err := tasks.Load(v2.root, "old-off"); err != nil || !g.Disabled || time.Duration(g.Timeout) != 30*time.Minute {
		t.Fatalf("a task off in V1 arrived as %+v (%v): off, and with V1's 30m it had without saying", g, err)
	}
	if _, err := os.Stat(tasks.Path(v2.root, "other-task")); err == nil {
		t.Fatal("another site's task came with it")
	}
	raw, _ := os.ReadFile(filepath.Join(v1, "tasks", "old-sync.json"))
	if !strings.Contains(string(raw), `"disabled": true`) || !strings.Contains(string(raw), `"why at 00:50"`) {
		t.Fatalf("the V1 task was not turned off, or lost what it said: %s", raw)
	}
	if raw, _ := os.ReadFile(filepath.Join(v1, "tasks", "other-task.json")); string(raw) != other {
		t.Fatalf("another site's V1 task was touched: %s", raw)
	}
}

// A V1 site with a site.v2.json comes in with that manifest, not a
// conversion, and its worker's data moves through the import token: the
// export where it was, and on the house the worker brought up on its own
// and its import run before the site is put in place.
func TestAV1SitesWorkerDataMovesThroughTheImportToken(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 stands in for the worker")
	}
	v2, _ := inboundFixture(t, &fakeDNS{enabled: true})
	_, out := v2.call("POST", "/api/v1/relocate/import-token", map[string]any{"domain": "old.test", "base": v2.http.URL})
	tok, _ := out["token"].(string)

	v1 := t.TempDir()
	site := filepath.Join(v1, "sites", "old.test")
	os.MkdirAll(filepath.Join(site, "public"), 0o755)
	os.WriteFile(filepath.Join(site, "site.json"), []byte(`{"title":"old","root":"public"}`), 0o644)
	os.WriteFile(filepath.Join(site, "data.txt"), []byte("rows written in v1"), 0o644)
	os.WriteFile(filepath.Join(site, relocate.V2ManifestName), []byte(`{"version":2,"static":{"root":"public"},
 "workers":{"db":{"command":["python3","-m","http.server","$PORT","--bind","127.0.0.1"],
  "move":{"export":["sh","-c","mkdir -p .move && cp data.txt .move/dump"],"ready":["true"],
   "import":["sh","-c","test -n \"$PORT\" && cp .move/dump imported.txt"]}}}}`), 0o644)

	src := &relocate.V1Source{Root: v1, Running: func(string) bool { return false }}
	notes, err := src.Convert("old.test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "site.v2.json") {
		t.Fatalf("notes do not say the manifest was taken as written: %v", notes)
	}
	m := &relocate.Mover{Root: v1, Source: src, Parts: relocate.ImportParts, Poll: 20 * time.Millisecond, ReadyWait: 5 * time.Second}
	var steps []string
	if _, err := m.Move(context.Background(), "old.test", relocate.HTTPTarget{Base: v2.http.URL, Token: tok}, func(s, _ string) { steps = append(steps, s) }); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(steps, " "); !strings.Contains(got, "stop export") || !strings.Contains(got, "final import") || strings.Index(got, "import") > strings.Index(got, "commit") {
		t.Fatalf("steps: %s", got)
	}
	if got, _ := os.ReadFile(filepath.Join(v2.root, "sites", "old.test", "imported.txt")); string(got) != "rows written in v1" {
		t.Fatalf("the import did not run there with the export's data: %q", got)
	}
	raw, _ := os.ReadFile(filepath.Join(v2.root, "sites", "old.test", "site.json"))
	if !strings.Contains(string(raw), `"workers"`) {
		t.Fatalf("the manifest arrived converted, not as site.v2.json said: %s", raw)
	}
}

// A name the house cannot point (an alias in a zone its DNS provider does
// not have) stops the import at the check, before the V1 site is stopped:
// at the end it was a name left pointing at a site that no longer served.
func TestAV1SiteWithANameTheHouseCannotPointIsRefusedBeforeItStops(t *testing.T) {
	v2, _ := inboundFixture(t, &fakeDNS{enabled: true, outside: map[string]bool{"other.test": true}})
	_, out := v2.call("POST", "/api/v1/relocate/import-token", map[string]any{"domain": "old.test", "base": v2.http.URL})
	tok, _ := out["token"].(string)
	v1 := t.TempDir()
	site := filepath.Join(v1, "sites", "old.test")
	os.MkdirAll(filepath.Join(site, "public"), 0o755)
	v1Manifest := `{"title":"old","root":"public","aliases":["other.test"]}`
	os.WriteFile(filepath.Join(site, "site.json"), []byte(v1Manifest), 0o644)
	src := &relocate.V1Source{Root: v1, Running: func(string) bool { return false }}
	if _, err := src.Convert("old.test"); err != nil {
		t.Fatal(err)
	}
	m := &relocate.Mover{Root: v1, Source: src, Parts: relocate.ImportParts, Poll: 20 * time.Millisecond, ReadyWait: 5 * time.Second}
	_, err := m.Move(context.Background(), "old.test", relocate.HTTPTarget{Base: v2.http.URL, Token: tok}, nil)
	if err == nil || !strings.Contains(err.Error(), "other.test") {
		t.Fatalf("err: %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(site, "site.json")); string(raw) != v1Manifest {
		t.Fatalf("the V1 site was touched: %s", raw)
	}
	if _, err := os.Stat(filepath.Join(v2.root, "data", "relocate", "old.test")); err == nil {
		t.Fatal("something was sent to a house that refused it")
	}
}
