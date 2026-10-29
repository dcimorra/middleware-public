package api

import (
	"context"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/helper"
	"github.com/dcimorra/the-middleware/internal/modules"
	"github.com/dcimorra/the-middleware/internal/modules/vpn"
)

// The VPN is the one module whose routes are mux patterns and whose
// Module shape differs from the registry's; this test enables it through
// the API over a real helper server (with a recorder in place of wg and
// nft) and drives its routes through the mount, so the adapter and the
// dispatcher are exercised the way the panel will.
func TestTheVPNModuleIsEnabledAndDrivenThroughTheModulesMount(t *testing.T) {
	f := newFixture(t).withToken()
	os.MkdirAll(filepath.Join(f.root, "data"), 0o755)
	dir := t.TempDir()
	srv := &helper.Server{
		Socket: filepath.Join(dir, "h.sock"), SocketGroup: -1, AllowUID: -1, AllowGID: -1,
		Exec:         &helper.Recorder{Outputs: map[string]string{}, Fail: map[string]error{}},
		Credentials:  func(net.Conn) (helper.Peer, error) { return helper.Peer{UID: 0}, nil },
		WireguardDir: filepath.Join(dir, "wireguard"),
		SysNetDir:    filepath.Join(dir, "net"),
		Log:          log.New(io.Discard, "", 0),
	}
	ln, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })

	box, err := modules.OpenBoxSecrets(f.root)
	if err != nil {
		t.Fatal(err)
	}
	v, err := vpn.New(vpn.Options{
		Root: f.root, Helper: helper.Dial(srv.Socket), Secrets: box, Bus: events.New(""),
		PanelPorts: []int{46100}, LookPath: func(string) (string, error) { return "/usr/bin/x", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := modules.New(f.root, f.bus)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Register(modules.VPNModule(v))
	f.srv.RegisterModules(ModulesOptions{Manager: mgr})

	resp, _ := f.call("GET", "/api/v1/modules/vpn/peers", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("routes of a disabled module: want 404, got %d", resp.StatusCode)
	}
	resp, body := f.call("POST", "/api/v1/modules/vpn/enable", map[string]any{"config": map[string]string{"endpoint": "vpn.example:51820"}})
	if resp.StatusCode != 200 || body["ok"] != true {
		t.Fatalf("enable: %d %s", resp.StatusCode, text(body))
	}
	resp, body = f.call("GET", "/api/v1/modules/vpn", nil)
	if resp.StatusCode != 200 || body["enabled"] != true || body["interface"] != "mw0" {
		t.Fatalf("status: %d %s", resp.StatusCode, text(body))
	}
	resp, body = f.call("POST", "/api/v1/modules/vpn/peers", map[string]string{"name": "laptop"})
	if resp.StatusCode != 201 || body["config"] == nil {
		t.Fatalf("add peer: %d %s", resp.StatusCode, text(body))
	}
	resp, _ = f.call("DELETE", "/api/v1/modules/vpn/peers/laptop", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("remove peer: %d", resp.StatusCode)
	}
	resp, _ = f.call("DELETE", "/api/v1/modules/vpn/peers/laptop", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("remove twice: %d", resp.StatusCode)
	}
	resp, _ = f.call("POST", "/api/v1/modules/vpn/disable", nil)
	if resp.StatusCode != 200 || v.Enabled() {
		t.Fatalf("disable: %d", resp.StatusCode)
	}
}
