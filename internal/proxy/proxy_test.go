package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/engine"
	"github.com/dcimorra/middleware-public/internal/manifest"
)

func lookupFor(sites map[string]engine.Snapshot) Lookup {
	return func(h string) (engine.Snapshot, bool) { s, ok := sites[h]; return s, ok }
}

func do(t *testing.T, h http.Handler, host, path string) *http.Response {
	t.Helper()
	r := httptest.NewRequest("GET", "http://"+host+path, nil)
	r.Host = host
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result()
}

func TestAnUnknownHostGetsTheNothingHerePage(t *testing.T) {
	h := New(lookupFor(nil))
	resp := do(t, h, "nobody.test", "/")
	if resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestWwwRedirectsToTheConfiguredName(t *testing.T) {
	h := New(lookupFor(map[string]engine.Snapshot{"a.test": {Domain: "a.test", Mode: manifest.ModeStatic}}))
	resp := do(t, h, "www.a.test", "/x?y=1")
	if resp.StatusCode != 301 || resp.Header.Get("Location") != "http://a.test/x?y=1" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestStaticNeverLeavesTheFolderNorServesDotfiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "public"), 0o755)
	os.WriteFile(filepath.Join(dir, "public", "index.html"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dir, "public", ".env"), []byte("SECRET=1"), 0o644)
	os.WriteFile(filepath.Join(dir, "site.json"), []byte("{}"), 0o644)
	m, _ := manifest.Parse([]byte(`{"version":2}`))
	h := New(lookupFor(map[string]engine.Snapshot{"a.test": {Domain: "a.test", Dir: dir, Mode: manifest.ModeStatic, Manifest: m}}))
	if resp := do(t, h, "a.test", "/"); resp.StatusCode != 200 {
		t.Fatalf("index: %d", resp.StatusCode)
	}
	for _, p := range []string{"/.env", "/../site.json", "/..%2fsite.json", "/.git/config"} {
		if resp := do(t, h, "a.test", p); resp.StatusCode == 200 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s served: %q", p, b)
		}
	}
}

func TestSPAFallsBackToIndex(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("app"), 0o644)
	m, _ := manifest.Parse([]byte(`{"version":2,"static":{"root":".","spa":true}}`))
	h := New(lookupFor(map[string]engine.Snapshot{"a.test": {Domain: "a.test", Dir: dir, Mode: manifest.ModeStatic, Manifest: m}}))
	resp := do(t, h, "a.test", "/deep/route")
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "app" {
		t.Fatalf("%d %q", resp.StatusCode, b)
	}
}

func TestRunForwardsWithTheOriginalHostAndForwardedHeaders(t *testing.T) {
	var gotHost, gotFor string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotFor = r.Host, r.Header.Get("X-Forwarded-For")
		w.Write([]byte("up"))
	}))
	defer up.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(up.URL, "http://127.0.0.1:"))
	m, _ := manifest.Parse([]byte(`{"version":2,"run":{"command":["x"]},"headers":{"X-Test":"1"}}`))
	h := New(lookupFor(map[string]engine.Snapshot{"a.test": {Domain: "a.test", Mode: manifest.ModeRun, Alive: true, Port: port, Manifest: m}}))
	r := httptest.NewRequest("GET", "http://a.test/", nil)
	r.Host = "a.test"
	r.Header.Set("CF-Connecting-IP", "203.0.113.9")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || gotHost != "a.test" || gotFor != "203.0.113.9" || w.Header().Get("X-Test") != "1" {
		t.Fatalf("code=%d host=%q for=%q hdr=%q", w.Code, gotHost, gotFor, w.Header().Get("X-Test"))
	}
}

func TestADeadInstanceGetsTheUnavailablePage(t *testing.T) {
	h := New(lookupFor(map[string]engine.Snapshot{"a.test": {Domain: "a.test", Mode: manifest.ModeRun, Alive: false}}))
	if resp := do(t, h, "a.test", "/"); resp.StatusCode != 503 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
