package api

import (
	"net"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/settings"
)

// settingsFixture keeps what PUT /settings saves, as the real hook does.
func settingsFixture(t *testing.T) (*fixture, *settings.Settings) {
	f := newFixture(t).withToken()
	saved := settings.Defaults()
	f.srv.opts.SaveSettings = func(s settings.Settings) error { saved = s; return nil }
	f.srv.opts.Settings = func() settings.Settings { return saved }
	return f, &saved
}

// TestThePanelDomainIsSavedAsTheProxyComparesIt: the proxy lowercases
// the request's host and compares it whole, and a name saved as typed
// locked the operator out of the panel the moment it was saved.
func TestThePanelDomainIsSavedAsTheProxyComparesIt(t *testing.T) {
	f, saved := settingsFixture(t)
	st := settings.Defaults()
	st.PanelDomain = " Panel.Example.COM. "
	if resp, body := f.call("PUT", "/api/v1/settings", st); resp.StatusCode != 200 {
		t.Fatalf("save: %d %s", resp.StatusCode, text(body))
	}
	if saved.PanelDomain != "panel.example.com" {
		t.Fatalf("saved %q", saved.PanelDomain)
	}
	for _, bad := range []string{"panel", "pa nel.example.com", "panel.example.com:8443", "panel..example.com", "-panel.example.com"} {
		st.PanelDomain = bad
		if resp, body := f.call("PUT", "/api/v1/settings", st); resp.StatusCode != 400 || !strings.Contains(text(body), "panel_domain") {
			t.Fatalf("%q: %d %s", bad, resp.StatusCode, text(body))
		}
	}
	if saved.PanelDomain != "panel.example.com" {
		t.Fatalf("a refused name was saved: %q", saved.PanelDomain)
	}
	// Empty is loopback only, and stays allowed.
	st.PanelDomain = ""
	if resp, body := f.call("PUT", "/api/v1/settings", st); resp.StatusCode != 200 || saved.PanelDomain != "" {
		t.Fatalf("empty: %d %s", resp.StatusCode, text(body))
	}
}

// TestAListenTheNextStartCannotBindIsRefused: "127.0.0.1" with no port
// was saved, and the restart failed in net.Listen over and over. A
// change is also bound and let go on the spot, so a port another
// program holds is said now and not at the restart.
func TestAListenTheNextStartCannotBindIsRefused(t *testing.T) {
	f, saved := settingsFixture(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freeAddr := free.Addr().String()
	free.Close()

	st := settings.Defaults()
	for _, bad := range []string{"127.0.0.1", "127.0.0.1:http", "127.0.0.1:0", "127.0.0.1:70000", held.Addr().String()} {
		st.Listen = bad
		if resp, body := f.call("PUT", "/api/v1/settings", st); resp.StatusCode != 400 || !strings.Contains(text(body), "listen") {
			t.Fatalf("%q: %d %s", bad, resp.StatusCode, text(body))
		}
	}
	if saved.Listen != settings.Defaults().Listen {
		t.Fatalf("a refused listen was saved: %q", saved.Listen)
	}
	st.Listen = freeAddr
	if resp, body := f.call("PUT", "/api/v1/settings", st); resp.StatusCode != 200 || saved.Listen != freeAddr {
		t.Fatalf("free port: %d %s saved %q", resp.StatusCode, text(body), saved.Listen)
	}
	// The address this process listens on is held by this process: it is
	// not a change, and binding it again would always fail.
	f.srv.opts.Listen = held.Addr().String()
	st.Listen = held.Addr().String()
	if resp, body := f.call("PUT", "/api/v1/settings", st); resp.StatusCode != 200 || saved.Listen != st.Listen {
		t.Fatalf("address in force: %d %s", resp.StatusCode, text(body))
	}
	// Nor is the one already saved and waiting for the restart.
	f.srv.opts.Listen = freeAddr
	if resp, body := f.call("PUT", "/api/v1/settings", st); resp.StatusCode != 200 {
		t.Fatalf("address saved: %d %s", resp.StatusCode, text(body))
	}
}
