package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/auth"
)

// unixClient serves h on a real unix socket, as the hook and the site
// door do, with base as the server's BaseContext, and returns a client
// that dials it.
func unixClient(t *testing.T, h http.Handler, base func(net.Listener) context.Context) *http.Client {
	t.Helper()
	// Short: a socket's path has a hundred-odd bytes, and a test's temp
	// folder is named after the test.
	dir, err := os.MkdirTemp("", "mwsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := ListenSocket(filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, BaseContext: base}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", ln.Addr().String())
	}}}
}

func get(t *testing.T, c *http.Client, path, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://localhost"+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// A request on a unix socket has no address, and any panel address list
// used to refuse it: every CLI command answered 403, and every site in a
// container lost its door. The house's socket gets past the lists; the
// containers' socket gets past them for the door it exists for, and for
// nothing else, as does a site's process on loopback.
func TestTheSocketsGetPastThePanelsLists(t *testing.T) {
	f, keys, _, _ := doorFixture(t, map[string]string{"c.test": `{"version":2,"claude_api":{}}`})
	if resp, body := f.call("PUT", "/api/v1/auth/panel-access", map[string]any{"allow": []string{"198.51.100.0/24"}}, "CF-Connecting-IP: 198.51.100.9"); resp.StatusCode != 200 {
		t.Fatalf("the list was not saved: %d %v", resp.StatusCode, body)
	}
	panel := f.srv.Panel()

	house := unixClient(t, panel, func(net.Listener) context.Context { return auth.WithSocket(context.Background()) })
	if code, me := get(t, house, "/api/v1/auth/me", ""); code != 200 || me["via"] != "socket" {
		t.Fatalf("the CLI's socket: %d %v", code, me)
	}

	door := unixClient(t, panel, nil)
	tok, _ := keys.Token("c.test")
	if code, me := get(t, door, "/api/site", tok); code != 200 || me["domain"] != "c.test" {
		t.Fatalf("a container's door: %d %v", code, me)
	}
	if code, _ := get(t, door, "/api/v1/auth/me", ""); code != 403 {
		t.Fatalf("the containers' socket reached the panel past its list: %d", code)
	}

	// Over loopback: the door, for a site's process; not the panel.
	f.token = tok
	if resp, out := f.call("GET", "/api/v1/site", nil); resp.StatusCode != 200 {
		t.Fatalf("a site's process on loopback lost its door: %d %v", resp.StatusCode, out)
	}
	f.token = ""
	if resp, _ := f.call("GET", "/api/v1/auth/me", nil); resp.StatusCode != 403 {
		t.Fatalf("plain loopback reached the panel past its list: %d", resp.StatusCode)
	}
	// Writing the edge's header from loopback is not the door either.
	f.token = tok
	if resp, out := f.call("GET", "/api/v1/site", nil, "CF-Connecting-IP: 203.0.113.9"); resp.StatusCode != 403 {
		t.Fatalf("a proxied caller off the list reached the door: %d %v", resp.StatusCode, out)
	}
}

// Closed to passwords is closed from loopback too, and the shell's
// opening is still the way back.
func TestPasskeyOnlyHoldsOnLoopbackUntilTheShellOpensThePanel(t *testing.T) {
	f := newFixture(t)
	if err := f.store.SetPassword("password-1"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(auth.PanelAccess{PasskeyOnly: true})
	if err := os.WriteFile(filepath.Join(f.root, "data", "auth", "panel-access.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	resp, body := f.call("POST", "/api/v1/auth/login", map[string]any{"password": "password-1"})
	if resp.StatusCode != 403 || !strings.Contains(text(body), "passkey") {
		t.Fatalf("a password from loopback with the password closed: %d %v", resp.StatusCode, body)
	}
	if _, err := f.store.OpenPanel(time.Minute); err != nil {
		t.Fatal(err)
	}
	if resp, body := f.call("POST", "/api/v1/auth/login", map[string]any{"password": "password-1"}); resp.StatusCode != 200 {
		t.Fatalf("opened from the shell: %d %v", resp.StatusCode, body)
	}
}
