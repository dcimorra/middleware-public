package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/helper"
	"github.com/dcimorra/the-middleware/internal/nodes"
	"github.com/dcimorra/the-middleware/internal/settings"
	"github.com/dcimorra/the-middleware/internal/wireguard"
)

// meshNet stands in for the kernel's interface; the mesh door and every
// call between the nodes are real HTTP on loopback addresses.
type meshNet struct {
	mu    sync.Mutex
	up    bool
	peers []wireguard.PeerStatus
}

func (f *meshNet) Apply(context.Context, wireguard.ServerConfig, bool) error {
	f.mu.Lock()
	f.up = true
	f.mu.Unlock()
	return nil
}
func (f *meshNet) Down(context.Context) error { return nil }
func (f *meshNet) Firewall(context.Context, helper.MeshFirewallArgs) error {
	return nil
}
func (f *meshNet) Status(context.Context) (helper.VPNStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return helper.VPNStatus{Up: f.up, Peers: f.peers}, nil
}

func meshPort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func nodeFixture(t *testing.T, name string, port int) (*fixture, *nodes.Mesh) {
	t.Helper()
	f := newFixture(t).withToken()
	m, err := nodes.Open(nodes.Options{Dir: filepath.Join(f.root, "data", "nodes"), Hostname: name, Version: "test", Net: &meshNet{}, Subnet: netip.MustParsePrefix("127.77.0.0/16"), APIPort: port})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.RegisterNodes(m, nil)
	m.Serve(f.srv)
	t.Cleanup(func() { _ = m.Leave(context.Background()) })
	return f, m
}

// Three nodes through their own APIs, as the panel does it: alpha invites
// beta, beta invites gamma, and alpha's panel works on gamma without
// anyone having linked the two.
func TestThreeNodesJoinedTwoByTwoAllSeeEachOther(t *testing.T) {
	port := meshPort(t)
	a, am := nodeFixture(t, "alpha", port)
	b, bm := nodeFixture(t, "beta", port)
	c, cm := nodeFixture(t, "gamma", port)

	res, inv := a.call("POST", "/api/v1/nodes/invite", map[string]string{"endpoint": "198.51.100.1:51821"})
	if res.StatusCode != 200 {
		t.Fatalf("invite: %d %s", res.StatusCode, text(inv))
	}
	if res, out := b.call("POST", "/api/v1/nodes/join", map[string]string{"code": inv["code"].(string)}); res.StatusCode != 200 {
		t.Fatalf("join: %d %s", res.StatusCode, text(out))
	}
	_, inv2 := b.call("POST", "/api/v1/nodes/invite", map[string]string{})
	if res, out := c.call("POST", "/api/v1/nodes/join", map[string]string{"code": inv2["code"].(string)}); res.StatusCode != 200 {
		t.Fatalf("join 2: %d %s", res.StatusCode, text(out))
	}
	am.Gossip(context.Background())
	bm.Gossip(context.Background())

	var cards []NodeCard
	getJSON(t, a, "/api/v1/nodes/overview", &cards)
	if len(cards) != 3 {
		t.Fatalf("alpha's overview: %+v", cards)
	}
	for _, card := range cards {
		if !card.OK {
			t.Fatalf("%s: %s", card.Name, card.Error)
		}
	}
	gammaID := cm.Self().ID

	// gamma's stream, read from alpha's panel, carries what gamma does.
	req, _ := http.NewRequest("GET", a.http.URL+"/api/v1/nodes/"+gammaID+"/api/events/stream", nil)
	req.Header.Set("Authorization", "Bearer "+a.token)
	stream, err := http.DefaultClient.Do(req)
	if err != nil || stream.StatusCode != 200 {
		t.Fatalf("stream: %v %v", err, stream)
	}
	defer stream.Body.Close()
	got := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(stream.Body)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "made-on-gamma") {
				got <- true
				return
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	res, site := a.call("POST", "/api/v1/nodes/"+gammaID+"/api/sites", map[string]any{"domain": "made-on-gamma.example", "template": "static", "publish": false})
	if res.StatusCode != 201 {
		t.Fatalf("create through the mesh: %d %s", res.StatusCode, text(site))
	}
	if res, _ := c.call("GET", "/api/v1/sites/made-on-gamma.example", nil); res.StatusCode != 200 {
		t.Fatal("the site is not on gamma")
	}
	if res, _ := a.call("GET", "/api/v1/sites/made-on-gamma.example", nil); res.StatusCode != 404 {
		t.Fatal("the site landed on alpha")
	}
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("gamma's event did not come through the mesh")
	}

	// Another node is an operator for sites, not for the network or the
	// credentials of this one.
	gm, _ := am.Member(gammaID)
	for _, p := range []string{"/auth/tokens", "/nodes", "/nodes/" + bm.Self().ID + "/api/sites"} {
		err := am.Call(context.Background(), gm, "GET", p, nil, nil)
		var re *nodes.RemoteError
		if !asRemote(err, &re) || re.Status != http.StatusForbidden {
			t.Fatalf("a node reached %s on another: %v", p, err)
		}
	}

	// beta removes gamma; alpha learns it in a round, gamma is out.
	if res, out := b.call("DELETE", "/api/v1/nodes/"+gammaID, nil); res.StatusCode != 200 {
		t.Fatalf("expel: %s", text(out))
	}
	am.Gossip(context.Background())
	if _, ok := am.Member(gammaID); ok {
		t.Fatal("alpha still has gamma")
	}
	time.Sleep(200 * time.Millisecond)
	if cm.InNetwork() {
		t.Fatal("gamma does not know it was removed")
	}
	if res, out := a.call("GET", "/api/v1/nodes/"+gammaID+"/api/sites", nil); res.StatusCode != 404 {
		t.Fatalf("the proxy still reaches gamma: %d %s", res.StatusCode, text(out))
	}
}

func TestAJoinWithoutHelperOrCodeSaysWhy(t *testing.T) {
	f := newFixture(t).withToken()
	m, err := nodes.Open(nodes.Options{Dir: t.TempDir(), Hostname: "x"})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.RegisterNodes(m, nil)
	if res, out := f.call("POST", "/api/v1/nodes/invite", map[string]string{"endpoint": "198.51.100.1:51821"}); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("%d %s", res.StatusCode, text(out))
	}
}

func asRemote(err error, re **nodes.RemoteError) bool {
	r, ok := err.(*nodes.RemoteError)
	if ok {
		*re = r
	}
	return ok
}

func getJSON(t *testing.T, f *fixture, path string, v any) {
	t.Helper()
	req, _ := http.NewRequest("GET", f.http.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

// A node in a network drops its panel's domain and is managed from the
// network; outside one, or with no other node answering, it is refused,
// because the panel would then be reachable only through an SSH tunnel.
func TestAPanelLeavesItsDomainOnlyWhereTheNetworkReachesIt(t *testing.T) {
	port := meshPort(t)
	a, _ := nodeFixture(t, "alpha", port)
	b := newFixture(t).withToken()
	bnet := &meshNet{}
	bm, err := nodes.Open(nodes.Options{Dir: filepath.Join(b.root, "data", "nodes"), Hostname: "beta", Version: "test", Net: bnet, Subnet: netip.MustParsePrefix("127.77.0.0/16"), APIPort: port})
	if err != nil {
		t.Fatal(err)
	}
	b.srv.RegisterNodes(bm, nil)
	bm.Serve(b.srv)
	t.Cleanup(func() { _ = bm.Leave(context.Background()) })
	st := settings.Defaults()
	st.PanelDomain = "beta.example"
	b.srv.opts.Settings = func() settings.Settings { return st }
	b.srv.opts.SaveSettings = func(n settings.Settings) error { st = n; return nil }

	if res, out := b.call("POST", "/api/v1/system/panel/network-only", nil); res.StatusCode != 409 || !strings.Contains(text(out), "no network") {
		t.Fatalf("outside a network: %d %s", res.StatusCode, text(out))
	}
	_, inv := a.call("POST", "/api/v1/nodes/invite", map[string]string{"endpoint": "198.51.100.1:51821"})
	if res, out := b.call("POST", "/api/v1/nodes/join", map[string]string{"code": inv["code"].(string)}); res.StatusCode != 200 {
		t.Fatalf("join: %d %s", res.StatusCode, text(out))
	}
	if res, out := b.call("POST", "/api/v1/system/panel/network-only", nil); res.StatusCode != 409 || !strings.Contains(text(out), "answers") {
		t.Fatalf("with no other node answering: %d %s", res.StatusCode, text(out))
	}
	if st.PanelDomain != "beta.example" {
		t.Fatal("the domain went although it was refused")
	}
	// alpha answers through the tunnel now.
	for _, x := range bm.Members() {
		bnet.mu.Lock()
		bnet.peers = append(bnet.peers, wireguard.PeerStatus{PublicKey: x.PublicKey, LastHandshake: time.Now()})
		bnet.mu.Unlock()
	}
	res, out := b.call("POST", "/api/v1/system/panel/network-only", nil)
	if res.StatusCode != 200 || st.PanelDomain != "" || !strings.Contains(text(out), "alpha") {
		t.Fatalf("in a network with alpha answering: %d %s (domain %q)", res.StatusCode, text(out), st.PanelDomain)
	}
}

// The move's plan is asked before it is confirmed: what it would do, and
// whether the target can take it.
func TestAMovesPlanIsReadBeforeItIsConfirmed(t *testing.T) {
	port := meshPort(t)
	a, _ := nodeFixture(t, "alpha", port)
	b, bm := nodeFixture(t, "beta", port)
	_, inv := a.call("POST", "/api/v1/nodes/invite", map[string]string{"endpoint": "198.51.100.1:51821"})
	if res, out := b.call("POST", "/api/v1/nodes/join", map[string]string{"code": inv["code"].(string)}); res.StatusCode != 200 {
		t.Fatalf("join: %s", text(out))
	}
	if res, out := a.call("POST", "/api/v1/sites", map[string]any{"domain": "p.test", "template": "static", "publish": false}); res.StatusCode != 201 {
		t.Fatalf("site: %s", text(out))
	}
	res, out := a.call("GET", "/api/v1/sites/p.test/move/plan?node="+bm.Self().ID, nil)
	if res.StatusCode != 200 || out["node"] != "beta" || out["check_error"] != nil {
		t.Fatalf("%d %s", res.StatusCode, text(out))
	}
	an, _ := out["analysis"].(map[string]any)
	if an == nil || an["files"].(float64) < 1 {
		t.Fatalf("analysis: %v", out)
	}
}
