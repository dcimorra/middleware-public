package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/httpcache"
	"github.com/dcimorra/the-middleware/internal/manifest"
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
	r.RemoteAddr = "127.0.0.1:5555" // the tunnel
	r.Header.Set("CF-Connecting-IP", "203.0.113.9")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || gotHost != "a.test" || gotFor != "203.0.113.9" || w.Header().Get("X-Test") != "1" {
		t.Fatalf("code=%d host=%q for=%q hdr=%q", w.Code, gotHost, gotFor, w.Header().Get("X-Test"))
	}
}

func TestTheManifestsHeadersReplaceTheSitesOwn(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Access-Control-Allow-Origin", "https://other.test")
		if r.URL.Path == "/account" {
			w.Header().Set("Cache-Control", "private, max-age=0")
		}
		w.Write([]byte("up"))
	}))
	defer up.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(up.URL, "http://127.0.0.1:"))
	m, _ := manifest.Parse([]byte(`{"version":2,"run":{"command":["x"]},"headers":{"Access-Control-Allow-Origin":"*","Cache-Control":"public, max-age=60","X-Frame-Options":"DENY"}}`))
	sites := map[string]engine.Snapshot{"a.test": {Domain: "a.test", Mode: manifest.ModeRun, Alive: true, Port: port, Manifest: m}}
	// The cache in front of it, as the house wires it: what it reads is
	// what the client gets.
	c := httpcache.New(64 << 20)
	h := c.Wrap(New(lookupFor(sites)), func(*http.Request) (httpcache.Site, bool) {
		return httpcache.Site{Domain: "a.test", Rules: []httpcache.Rule{{Path: "/", Mode: httpcache.ModeOrigin}}, Generation: "g"}, true
	})

	resp := do(t, h, "a.test", "/account")
	if v := resp.Header.Values("Access-Control-Allow-Origin"); len(v) != 1 || v[0] != "*" {
		t.Fatalf("Access-Control-Allow-Origin %q: two of them and CORS fails", v)
	}
	cc := resp.Header.Values("Cache-Control")
	if len(cc) != 1 || !strings.Contains(cc[0], "max-age=60") || !strings.Contains(cc[0], "private") {
		t.Fatalf("Cache-Control %q: the manifest's, with the site's private kept", cc)
	}
	if v := resp.Header.Values("X-Frame-Options"); len(v) != 1 {
		t.Fatalf("X-Frame-Options %q", v)
	}
	if do(t, h, "a.test", "/account").Header.Get("X-Middleware-Cache") == "hit" || calls.Load() != 2 {
		t.Fatal("a page the site marked private was kept on the manifest's word")
	}
	// A page the site said nothing about is the manifest's to mark.
	do(t, h, "a.test", "/")
	if r := do(t, h, "a.test", "/"); r.Header.Get("X-Middleware-Cache") != "hit" {
		t.Fatalf("the manifest's Cache-Control did not reach the cache: %q", r.Header.Get("X-Middleware-Cache"))
	}

	// What the house answers itself carries them too: a static file, and
	// the page of a site that is not answering.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("hello"), 0o644)
	sm, _ := manifest.Parse([]byte(`{"version":2,"static":{"root":"."},"headers":{"X-Frame-Options":"DENY"},"cache":{"static_max_age":"1h"}}`))
	sites["s.test"] = engine.Snapshot{Domain: "s.test", Dir: dir, Mode: manifest.ModeStatic, Manifest: sm}
	sites["down.test"] = engine.Snapshot{Domain: "down.test", Mode: manifest.ModeRun, Alive: true, Port: 1, Manifest: m}
	p := New(lookupFor(sites))
	resp = do(t, p, "s.test", "/")
	if resp.StatusCode != 200 || resp.Header.Get("X-Frame-Options") != "DENY" || resp.Header.Get("Cache-Control") != "public, max-age=3600" {
		t.Fatalf("static: %d %v", resp.StatusCode, resp.Header)
	}
	if resp := do(t, p, "down.test", "/"); resp.StatusCode != 503 || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("down: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestADirectClientCannotTellASiteWhoItIs(t *testing.T) {
	var got http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	defer up.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(up.URL, "http://127.0.0.1:"))
	m, _ := manifest.Parse([]byte(`{"version":2,"run":{"command":["x"]}}`))
	h := New(lookupFor(map[string]engine.Snapshot{"a.test": {Domain: "a.test", Mode: manifest.ModeRun, Alive: true, Port: port, Manifest: m}}))
	r := httptest.NewRequest("GET", "http://a.test/", nil)
	r.Host = "a.test"
	r.RemoteAddr = "198.51.100.7:4000" // the 80/443 door
	for _, k := range []string{"CF-Connecting-IP", "X-Forwarded-For", "X-Real-IP", "True-Client-IP"} {
		r.Header.Set(k, "10.0.0.1")
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	h.ServeHTTP(httptest.NewRecorder(), r)
	for _, k := range []string{"CF-Connecting-IP", "X-Forwarded-For", "X-Real-IP"} {
		if got.Get(k) != "198.51.100.7" {
			t.Errorf("%s reached the site as %q", k, got.Get(k))
		}
	}
	if got.Get("True-Client-IP") != "" || got.Get("X-Forwarded-Proto") != "http" {
		t.Errorf("forged headers reached the site: %v", got)
	}
}

func TestStaticDoesNotFollowALinkOutOfTheSite(t *testing.T) {
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "id_rsa"), []byte("PRIVATE"), 0o600)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "public"), 0o755)
	os.Symlink(outside, filepath.Join(dir, "public", "keys"))
	os.Symlink(filepath.Join(outside, "id_rsa"), filepath.Join(dir, "public", "key.txt"))
	m, _ := manifest.Parse([]byte(`{"version":2}`))
	h := New(lookupFor(map[string]engine.Snapshot{"a.test": {Domain: "a.test", Dir: dir, Mode: manifest.ModeStatic, Manifest: m}}))
	for _, p := range []string{"/keys/id_rsa", "/key.txt"} {
		if resp := do(t, h, "a.test", p); resp.StatusCode == 200 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s served through a link: %q", p, b)
		}
	}
}

func TestADeadInstanceGetsTheUnavailablePage(t *testing.T) {
	h := New(lookupFor(map[string]engine.Snapshot{"a.test": {Domain: "a.test", Mode: manifest.ModeRun, Alive: false}}))
	if resp := do(t, h, "a.test", "/"); resp.StatusCode != 503 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestAProjectAnswersNoHostname(t *testing.T) {
	h := New(lookupFor(map[string]engine.Snapshot{"tools": {Domain: "tools", Mode: manifest.ModeProject}}))
	if resp := do(t, h, "tools", "/"); resp.StatusCode != 404 {
		t.Fatalf("a project answered %d", resp.StatusCode)
	}
}

func TestStartingAndDownAreToldApartAndHoldModesAnswerTheirCode(t *testing.T) {
	m, _ := manifest.Parse([]byte(`{"version":2,"title":"Shop","run":{"command":["x"]},"notice":{"theme":"card"}}`))
	cases := []struct {
		sn         engine.Snapshot
		code       int
		want       string
		retryAfter string
	}{
		{engine.Snapshot{Domain: "a.test", Mode: manifest.ModeRun, Manifest: m}, 503, "Starting", "5"},
		{engine.Snapshot{Domain: "a.test", Mode: manifest.ModeRun, Manifest: m, Error: "exit 1"}, 503, "Not answering", "30"},
		{engine.Snapshot{Domain: "a.test", Mode: manifest.ModeDisabled, Manifest: m}, 503, "Switched off", ""},
	}
	until := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	for _, mode := range []struct {
		mode, label string
		code        int
	}{{"", "Maintenance", 503}, {"404", "Not found", 404}, {"410", "Gone", 410}, {"500", "Error", 500}} {
		hm, _ := manifest.Parse([]byte(`{"version":2,"title":"Shop","hold":{"enabled":true,"mode":"` + mode.mode + `","until":"` + until + `","message":"Back after the move"}}`))
		retry := ""
		if mode.mode == "" {
			retry = "7200"
		}
		cases = append(cases, struct {
			sn         engine.Snapshot
			code       int
			want       string
			retryAfter string
		}{engine.Snapshot{Domain: "a.test", Mode: manifest.ModeHold, Manifest: hm}, mode.code, mode.label, retry})
	}
	for _, c := range cases {
		h := New(lookupFor(map[string]engine.Snapshot{"a.test": c.sn}))
		resp := do(t, h, "a.test", "/")
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != c.code || !strings.Contains(string(b), c.want) || !strings.Contains(string(b), "Shop") {
			t.Errorf("%s/%s: %d, page without %q", c.sn.Mode, c.want, resp.StatusCode, c.want)
		}
		if got := resp.Header.Get("Retry-After"); c.retryAfter != "" && (got == "" || (c.retryAfter == "7200" && got < "7100")) || c.retryAfter == "5" && got != "5" {
			t.Errorf("%s/%s: Retry-After %q", c.sn.Mode, c.want, got)
		}
	}
}

func TestAnUpstreamThatFailsGets503NotACloudflare502(t *testing.T) {
	m, _ := manifest.Parse([]byte(`{"version":2,"run":{"command":["x"]}}`))
	h := New(lookupFor(map[string]engine.Snapshot{"a.test": {Domain: "a.test", Mode: manifest.ModeRun, Alive: true, Port: 1, Manifest: m}}))
	if resp := do(t, h, "a.test", "/"); resp.StatusCode != 503 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// A site's redirects answer before it is served, even stopped, and one
// that would send a request to itself is not followed.
func TestASitesRedirectsAnswerBeforeItIsServed(t *testing.T) {
	m := &manifest.Manifest{Redirects: []manifest.Redirect{
		{From: "/old/*", To: "https://new.test/*", Code: 308},
		{From: "alias.test/*", To: "https://a.test/*"},
		{From: "/self", To: "/self"},
	}}
	sites := map[string]engine.Snapshot{
		"a.test":     {Domain: "a.test", Mode: manifest.ModeDisabled, Manifest: m},
		"alias.test": {Domain: "a.test", Mode: manifest.ModeDisabled, Manifest: m},
	}
	h := New(lookupFor(sites))
	resp := do(t, h, "a.test", "/old/page?q=1")
	if resp.StatusCode != 308 || resp.Header.Get("Location") != "https://new.test/page?q=1" {
		t.Fatalf("path: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp = do(t, h, "alias.test", "/x")
	if resp.StatusCode != 301 || resp.Header.Get("Location") != "https://a.test/x" {
		t.Fatalf("alias: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := do(t, h, "a.test", "/self"); resp.StatusCode/100 == 3 {
		t.Fatalf("a loop was followed: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Anything else is the site as it is: stopped.
	if resp := do(t, h, "a.test", "/other"); resp.StatusCode/100 == 3 {
		t.Fatalf("a path with no rule was redirected: %d", resp.StatusCode)
	}
}
