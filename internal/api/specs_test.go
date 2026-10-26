package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestTheSpecsTabReadsTheFolderAndSetsUpOnlyWithTheModuleOn(t *testing.T) {
	f := newFixture(t).withToken()
	dir := filepath.Join(f.root, "sites", "s.test")
	os.MkdirAll(filepath.Join(dir, "openspec", "changes", "add-x"), 0o755)
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"static":{"root":"public"}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "openspec", "changes", "add-x", "tasks.md"), []byte("- [x] 1.1 a\n- [ ] 1.2 b\n"), 0o644)
	on, ran := false, ""
	f.srv.RegisterSpecs(SpecsOptions{Enabled: func() bool { return on }, Init: func(ctx context.Context, domain string) (string, error) {
		ran = domain
		return "done", nil
	}})
	resp, d := f.call("GET", "/api/v1/sites/s.test/specs", nil)
	if resp.StatusCode != 200 || d["initialized"] != true || d["enabled"] != false {
		t.Fatalf("%d %v", resp.StatusCode, d)
	}
	ch := d["changes"].([]any)[0].(map[string]any)
	if ch["id"] != "add-x" || ch["done"].(float64) != 1 || ch["total"].(float64) != 2 {
		t.Fatalf("the change: %v", ch)
	}
	// Off: refused, and nothing ran.
	if resp, _ := f.call("POST", "/api/v1/sites/s.test/specs/init", nil); resp.StatusCode != 409 || ran != "" {
		t.Fatalf("init with the module off: %d, ran %q", resp.StatusCode, ran)
	}
	on = true
	if resp, _ := f.call("POST", "/api/v1/sites/s.test/specs/init", nil); resp.StatusCode != 200 || ran != "s.test" {
		t.Fatalf("init: %d, ran %q", resp.StatusCode, ran)
	}
	if resp, _ := f.call("GET", "/api/v1/sites/nope.test/specs", nil); resp.StatusCode != 404 {
		t.Fatalf("a site that is not there: %d", resp.StatusCode)
	}
}
