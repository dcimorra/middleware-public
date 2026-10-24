package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/relocate"
)

// A V1 site comes into a V2 house with the token the V2 panel made for it:
// converted, put in place, and stopped in V1. The token opens that site's
// import and nothing else.
func TestAV1SiteIsImportedWithItsOneSiteToken(t *testing.T) {
	v2 := newFixture(t).withToken()
	_, out := v2.call("POST", "/api/v1/relocate/import-token", map[string]any{"domain": "old.test", "base": v2.http.URL})
	tok, _ := out["token"].(string)
	if !strings.HasPrefix(tok, "mw_") || !strings.Contains(out["command"].(string), "import --to") {
		t.Fatalf("import token: %v", out)
	}

	v1 := t.TempDir()
	site := filepath.Join(v1, "sites", "old.test")
	os.MkdirAll(filepath.Join(site, "public"), 0o755)
	os.WriteFile(filepath.Join(site, "public", "index.html"), []byte("from v1"), 0o644)
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
	v2 := newFixture(t).withToken()
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
	m := &relocate.Mover{Root: filepath.Dir(app), Dir: app, Source: src, Parts: src.Parts, Poll: 20 * time.Millisecond, ReadyWait: 5 * time.Second}
	if _, err := m.Move(context.Background(), "shop.test", relocate.HTTPTarget{Base: v2.http.URL, Token: tok, Install: true}, nil); err != nil {
		t.Fatal(err)
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
