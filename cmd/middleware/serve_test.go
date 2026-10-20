package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	sections := []string{"/sites", "/events", "/system", "/modules", "/inbound", "/inbound/certificates", "/waf", "/waf/traffic", "/agents", "/agents/capacity", "/chat/middleware", "/tasks", "/guard", "/openapi.json"}
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
	resp, err := http.Get("http://" + s.Listen + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 0 || resp.StatusCode >= 500 {
		t.Fatalf("unknown host through the chain: %d", resp.StatusCode)
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
