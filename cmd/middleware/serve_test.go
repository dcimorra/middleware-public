package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/api"
	"github.com/dcimorra/middleware-public/internal/app"
	"github.com/dcimorra/middleware-public/internal/settings"
)

// freePort asks the kernel for a port nobody holds.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// TestServeAnswersEverySectionOfTheAPI builds the process the way `serve`
// does, hooks and all, and asks every section of docs/api.md for its
// listing over the unix socket. It is the one place where the wiring is
// tested by running it: a hook built in the wrong order or a route
// registered on the wrong server shows up here as a 404 or a 500, not in
// a unit test that mounts the section by hand.
func TestServeAnswersEverySectionOfTheAPI(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "data"), 0o755)
	s := settings.Defaults()
	s.Listen = freePort(t)
	if err := settings.Save(root, s); err != nil {
		t.Fatal(err)
	}
	// A static site with a security section, asked through the listener
	// further down: the door is a hook, and only the wired chain proves it
	// sits where it should.
	shop := filepath.Join(root, "sites", "shop.example")
	os.MkdirAll(shop, 0o755)
	os.WriteFile(filepath.Join(shop, "index.html"), []byte("<p>shop</p>"), 0o644)
	os.WriteFile(filepath.Join(shop, "site.json"), []byte(`{"version":2,"security":{"bots":{"deny":["ai"]},"paths":["/wp-login.php"]}}`), 0o644)
	a, err := app.New(root)
	if err != nil {
		t.Fatal(err)
	}
	a.Hooks = hooks(a)
	if len(a.Hooks) < 9 {
		t.Fatalf("only %d hooks were built", len(a.Hooks))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("serve did not stop")
		}
	})

	sock := api.SocketPath(root)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the api socket never appeared")
		}
		time.Sleep(50 * time.Millisecond)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := client.Get("http://unix" + api.Prefix + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	sections := []string{"/sites", "/events", "/system", "/modules", "/inbound", "/inbound/certificates", "/waf", "/waf/traffic", "/security/catalog", "/agents", "/agents/capacity", "/chat/middleware", "/tasks", "/guard", "/openapi.json"}
	for _, path := range sections {
		code, body := get(path)
		if code != 200 {
			t.Errorf("GET %s: %d %s", path, code, body)
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Errorf("GET %s: not JSON: %v\n%s", path, err, body)
		}
	}

	// The modules the wiring adds are listed, and the module routes
	// resolve through the mount rather than through the catch-all.
	_, body := get("/modules")
	for _, key := range []string{"cloudflare", "github", "vpn", "agent-claude", "agent-gemini"} {
		if !strings.Contains(body, `"key": "`+key+`"`) {
			t.Errorf("module %s is not listed:\n%s", key, body)
		}
	}
	if code, _ := get("/modules/vpn/peers"); code != 404 {
		t.Errorf("a disabled module's route must be 404, got %d", code)
	}

	// The request chain: a host nobody serves gets the proxy's answer
	// through inbound → waf → access → traffic, not a crash in one of them.
	// The API socket opens before the loopback listener, so the listener
	// is given a moment rather than assumed.
	var resp *http.Response
	for wait := time.Now().Add(10 * time.Second); ; {
		var err error
		if resp, err = http.Get("http://" + s.Listen + "/"); err == nil {
			break
		} else if time.Now().After(wait) {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	resp.Body.Close()
	if resp.StatusCode == 0 || resp.StatusCode >= 500 {
		t.Fatalf("unknown host through the chain: %d", resp.StatusCode)
	}

	// The security door, through the whole chain.
	ask := func(path, ua string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://"+s.Listen+path, nil)
		req.Host = "shop.example"
		req.Header.Set("User-Agent", ua)
		for wait := time.Now().Add(10 * time.Second); ; {
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			// The engine finds the site within a scan; until then the
			// host is unknown.
			if resp.StatusCode != http.StatusNotFound || path != "/" || time.Now().After(wait) {
				return resp.StatusCode
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	browser := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"
	if code := ask("/", browser); code != 200 {
		t.Fatalf("a browser on the shop got %d", code)
	}
	if code := ask("/", "Mozilla/5.0 (compatible; GPTBot/1.2; +https://openai.com/gptbot)"); code != 403 {
		t.Errorf("an AI crawler got %d", code)
	}
	for _, p := range []string{"/.git/config", "/a/../.env", "/wp-login.php"} {
		if code := ask(p, browser); code != 404 {
			t.Errorf("%s got %d", p, code)
		}
	}
	if code, body := get("/sites/shop.example/security/blocks"); code != 200 || !strings.Contains(body, "GPTBot") || !strings.Contains(body, `".git/"`) {
		t.Errorf("the refusals are not listed: %d %s", code, body)
	}

	// The ready event carries what was wired.
	code, body := get("/events?type=middleware.ready")
	if code != 200 || !strings.Contains(body, `"middleware.ready"`) {
		t.Fatalf("middleware.ready was not emitted: %d %s", code, body)
	}

	// `middleware agents` reads the same answer over the socket, and the
	// capacity it prints is the settings' default.
	code, body = get("/agents")
	if code != 200 || !strings.Contains(body, `"max": 8`) || !strings.Contains(body, `"provider": "claude"`) {
		t.Fatalf("agents: %d %s", code, body)
	}
	if err := agentsCmd([]string{"--root", root}); err != nil {
		t.Fatalf("middleware agents: %v", err)
	}

	// docs/api.md and the routes of the whole wired process say the same
	// thing, both ways: a route added without its line breaks this, and so
	// does a line left for a route that is gone. V1 learned that a
	// hand-written reference goes stale within months otherwise.
	_, body = get("/openapi.json")
	var spec struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal([]byte(body), &spec); err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{}
	for path, methods := range spec.Paths {
		for m := range methods {
			switch strings.ToUpper(m) {
			case "GET", "POST", "PUT", "PATCH", "DELETE":
				live[strings.ToUpper(m)+" "+strings.TrimPrefix(path, api.Prefix)] = true
			}
		}
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "api.md"))
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	row := regexp.MustCompile("(?m)^\\| (GET|POST|PUT|PATCH|DELETE) \\| `([^`]+)` \\|")
	for _, m := range row.FindAllStringSubmatch(string(doc), -1) {
		documented[m[1]+" "+m[2]] = true
	}
	var missing, stale []string
	for r := range live {
		if !documented[r] {
			missing = append(missing, r)
		}
	}
	for r := range documented {
		// A module's own routes are served through its mount, which the
		// registry lists as one entry; they are checked by the module.
		if _, path, _ := strings.Cut(r, " "); strings.HasPrefix(path, "/modules/vpn") || strings.HasPrefix(path, "/modules/{agent}") {
			continue
		}
		if !live[r] {
			stale = append(stale, r)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 || len(stale) > 0 {
		t.Errorf("docs/api.md is out of step with the routes.\nnot documented:\n  %s\ndocumented but gone:\n  %s", strings.Join(missing, "\n  "), strings.Join(stale, "\n  "))
	}
}

// TestMiddlewareListenOverridesTheSettings is the docker contract: the
// image sets MIDDLEWARE_LISTEN to 0.0.0.0 because a published port never
// reaches the container's own loopback, and the settings file must not
// have to change for that.
func TestMiddlewareListenOverridesTheSettings(t *testing.T) {
	t.Setenv("MIDDLEWARE_LISTEN", "")
	if got := app.ListenAddr("127.0.0.1:46100"); got != "127.0.0.1:46100" {
		t.Fatalf("unset variable must keep the settings, got %q", got)
	}
	t.Setenv("MIDDLEWARE_LISTEN", "0.0.0.0:46100")
	if got := app.ListenAddr("127.0.0.1:46100"); got != "0.0.0.0:46100" {
		t.Fatalf("the variable must win, got %q", got)
	}
}
