package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/modules"
)

// fakeModule is a module with no needs whose routes are declared in the
// style the test picks: plain keys like github's, or mux patterns like
// the vpn's.
type fakeModule struct {
	key      string
	patterns bool
	enabled  bool
}

func (m *fakeModule) Key() string         { return m.key }
func (m *fakeModule) Name() string        { return m.key }
func (m *fakeModule) Description() string { return "fake" }
func (m *fakeModule) Needs() []modules.Need {
	return []modules.Need{{Key: "x", Label: "x", Optional: true}}
}
func (m *fakeModule) Verify(context.Context, modules.Config) []modules.Line {
	return []modules.Line{modules.OK("probed")}
}
func (m *fakeModule) Install(_ context.Context, _ modules.Config, progress func(string)) error {
	progress("installed nothing")
	return nil
}
func (m *fakeModule) Enable(context.Context, modules.Config) error { m.enabled = true; return nil }
func (m *fakeModule) Disable(context.Context) error                { m.enabled = false; return nil }
func (m *fakeModule) Routes() map[string]http.Handler {
	echo := func(text string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			WriteJSON(w, 200, map[string]string{"route": text, "name": r.PathValue("name")})
		})
	}
	if m.patterns {
		return map[string]http.Handler{"GET /{$}": echo("status"), "DELETE /peers/{name}": echo("peer")}
	}
	return map[string]http.Handler{"hook": echo("hook"), "status": echo("status")}
}

func modulesFixture(t *testing.T) (*fixture, *modules.Manager) {
	t.Helper()
	f := newFixture(t).withToken()
	os.MkdirAll(filepath.Join(f.root, "data"), 0o755)
	mgr, err := modules.New(f.root, f.bus)
	if err != nil {
		t.Fatal(err)
	}
	return f, mgr
}

func TestModulesAreListedEnabledAndTheirRoutesMountedBehindTheirState(t *testing.T) {
	f, mgr := modulesFixture(t)
	plain := &fakeModule{key: "plain"}
	mgr.Register(plain)
	f.srv.RegisterModules(ModulesOptions{Manager: mgr, Public: []string{"POST plain/hook"}})

	resp, body := f.call("GET", "/api/v1/modules", nil)
	if resp.StatusCode != 200 || !strings.Contains(text(body), `"key": "plain"`) {
		t.Fatalf("list: %d %s", resp.StatusCode, text(body))
	}
	resp, _ = f.call("GET", "/api/v1/modules/plain/status", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("a disabled module's route must be 404, got %d", resp.StatusCode)
	}
	resp, body = f.call("POST", "/api/v1/modules/plain/verify", map[string]any{"config": map[string]string{"x": "1"}})
	if resp.StatusCode != 200 || body["ok"] != true {
		t.Fatalf("verify: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("POST", "/api/v1/modules/plain/enable", map[string]any{"config": map[string]string{"x": "1"}})
	if resp.StatusCode != 200 || body["ok"] != true || !plain.enabled {
		t.Fatalf("enable: %d %v", resp.StatusCode, body)
	}
	if progress, _ := body["progress"].([]any); len(progress) != 1 || progress[0] != "installed nothing" {
		t.Fatalf("enable must return the install's progress lines: %v", body["progress"])
	}
	resp, body = f.call("GET", "/api/v1/modules/plain/status", nil)
	if resp.StatusCode != 200 || body["route"] != "status" {
		t.Fatalf("mounted route: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("GET", "/api/v1/modules/plain", nil)
	if resp.StatusCode != 200 || body["enabled"] != true {
		t.Fatalf("one module: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("POST", "/api/v1/modules/plain/enable", map[string]any{"config": map[string]string{"nope": "1"}})
	if resp.StatusCode != 400 {
		t.Fatalf("an unknown need must be 400: %d %v", resp.StatusCode, body)
	}
	resp, _ = f.call("POST", "/api/v1/modules/plain/disable", nil)
	if resp.StatusCode != 200 || plain.enabled {
		t.Fatalf("disable: %d", resp.StatusCode)
	}
	resp, _ = f.call("DELETE", "/api/v1/modules/plain/credentials", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("forget: %d", resp.StatusCode)
	}
	resp, _ = f.call("POST", "/api/v1/modules/missing/verify", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown module: %d", resp.StatusCode)
	}
}

func TestAPublicModuleRouteNeedsNoCallerButOnlyWhileTheModuleIsOn(t *testing.T) {
	f, mgr := modulesFixture(t)
	mgr.Register(&fakeModule{key: "plain"})
	f.srv.RegisterModules(ModulesOptions{Manager: mgr, Public: []string{"POST plain/hook"}})
	f.token = ""
	resp, _ := f.call("POST", "/api/v1/modules/plain/hook", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("hook of a disabled module: want 404, got %d", resp.StatusCode)
	}
	if _, err := mgr.Enable(context.Background(), "plain", nil, nil); err != nil {
		t.Fatal(err)
	}
	resp, body := f.call("POST", "/api/v1/modules/plain/hook", nil)
	if resp.StatusCode != 200 || body["route"] != "hook" {
		t.Fatalf("public hook: %d %v", resp.StatusCode, body)
	}
	resp, _ = f.call("GET", "/api/v1/modules/plain/status", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("the other routes stay behind authentication: %d", resp.StatusCode)
	}
}

func TestPatternStyleModuleRoutesResolveThroughAMux(t *testing.T) {
	f, mgr := modulesFixture(t)
	mgr.Register(&fakeModule{key: "pat", patterns: true})
	f.srv.RegisterModules(ModulesOptions{Manager: mgr})
	if _, err := mgr.Enable(context.Background(), "pat", nil, nil); err != nil {
		t.Fatal(err)
	}
	resp, body := f.call("GET", "/api/v1/modules/pat", nil)
	if resp.StatusCode != 200 || body["route"] != "status" {
		t.Fatalf("GET /modules/{key} must reach the module's own status: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("DELETE", "/api/v1/modules/pat/peers/laptop", nil)
	if resp.StatusCode != 200 || body["name"] != "laptop" {
		t.Fatalf("pattern route: %d %v", resp.StatusCode, body)
	}
	resp, _ = f.call("GET", "/api/v1/modules/pat/nothing", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown pattern route: %d", resp.StatusCode)
	}
}
