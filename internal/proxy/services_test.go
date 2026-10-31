package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// The proxy asks for the request's host and path together, and sends a
// site's names to each one's service, the headers an app builds its
// redirect_uri from with them: the real Host, the client the house saw,
// and the scheme the house knows, never the client's.
func TestEachNameReachesItsServiceWithTheHeadersAnAppNeeds(t *testing.T) {
	m, err := manifest.Parse([]byte(`{"version":2,"services":{
		"web":{"run":{"command":["x"]},"domains":["shop.test"]},
		"api":{"run":{"command":["y"]},"domains":["shop.test/api"]},
		"cms":{"run":{"command":["z"]},"domains":["cms.shop.test"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]http.Header{}
	serve := func(name string) int {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Clone()
			h.Set("Host", r.Host)
			seen[name] = h
			io.WriteString(w, name)
		}))
		t.Cleanup(s.Close)
		p, _ := strconv.Atoi(strings.TrimPrefix(s.URL, "http://127.0.0.1:"))
		return p
	}
	ports := map[string]int{"web": serve("web"), "api": serve("api")}
	h := New(func(string) (engine.Snapshot, bool) { return engine.Snapshot{}, false })
	h.ByPath = func(host, path string) (engine.Snapshot, bool) {
		svc, ok := m.ServiceFor(host, path)
		if !ok {
			return engine.Snapshot{}, false
		}
		sn := engine.Snapshot{Domain: "shop.test", Mode: manifest.ModeRun, Manifest: m, Service: svc}
		if p := ports[svc]; p != 0 {
			sn.Alive, sn.Port = true, p
		} else {
			sn.Handing = true // cms is still installing
		}
		return sn, true
	}
	cases := []struct{ host, path, body string }{
		{"shop.test", "/", "web"}, {"shop.test", "/api/orders", "api"}, {"shop.test", "/apix", "web"},
	}
	for _, c := range cases {
		resp := do(t, h, c.host, c.path)
		b, _ := io.ReadAll(resp.Body)
		if string(b) != c.body {
			t.Errorf("%s%s answered %q, want %q", c.host, c.path, b, c.body)
		}
	}
	r := httptest.NewRequest("GET", "http://shop.test/api/login", nil)
	r.Host = "shop.test"
	r.RemoteAddr = "198.51.100.7:4000"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-For", "10.9.9.9")
	h.ServeHTTP(httptest.NewRecorder(), r)
	got := seen["api"]
	if got.Get("Host") != "shop.test" || got.Get("X-Forwarded-For") != "198.51.100.7" || got.Get("X-Forwarded-Host") != "shop.test" || got.Get("X-Forwarded-Proto") != "http" {
		t.Errorf("the service saw %v", got)
	}

	// cms is starting: its name gets the house's page, never a raw 502,
	// and the others do not change.
	resp := do(t, h, "cms.shop.test", "/")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Errorf("a starting service's name answered %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if resp := do(t, h, "elsewhere.test", "/"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a name no service answers got %d, want the house's 404", resp.StatusCode)
	}
}
