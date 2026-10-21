package nodes

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/helper"
	"github.com/dcimorra/middleware-public/internal/wireguard"
)

// fakeNet is the interface without the kernel: it records what the mesh
// asked for. The door and the calls are real, on loopback addresses drawn
// from 127.77.0.0/16, which Linux answers on without configuring anything.
type fakeNet struct {
	mu   sync.Mutex
	cfg  wireguard.ServerConfig
	up   bool
	fw   helper.MeshFirewallArgs
	ups  int
	down int
}

func (f *fakeNet) Apply(_ context.Context, cfg wireguard.ServerConfig, restart bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := wireguard.RenderServer(cfg); err != nil {
		return err
	}
	f.cfg, f.up = cfg, true
	f.ups++
	return nil
}
func (f *fakeNet) Down(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.up = false
	f.down++
	return nil
}
func (f *fakeNet) Firewall(_ context.Context, a helper.MeshFirewallArgs) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fw = a
	return nil
}
func (f *fakeNet) Status(context.Context) (helper.VPNStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := helper.VPNStatus{Up: f.up}
	for _, p := range f.cfg.Peers {
		st.Peers = append(st.Peers, wireguard.PeerStatus{PublicKey: p.PublicKey, LastHandshake: time.Now()})
	}
	return st, nil
}
func (f *fakeNet) peers() map[netip.Addr]wireguard.Key {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[netip.Addr]wireguard.Key{}
	for _, p := range f.cfg.Peers {
		out[p.AllowedIPs[0].Addr()] = p.PublicKey
	}
	return out
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type node struct {
	*Mesh
	net *fakeNet
}

func newNode(t *testing.T, name string, port int) node {
	t.Helper()
	f := &fakeNet{}
	m, err := Open(Options{Dir: t.TempDir(), Hostname: name, Version: "test", Net: f, Subnet: netip.MustParsePrefix("127.77.0.0/16"), APIPort: port})
	if err != nil {
		t.Fatal(err)
	}
	// What the API would answer: who asked, as the door saw them.
	m.Serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who, _ := MemberFrom(r.Context())
		doorJSON(w, 200, map[string]string{"from": who.Name, "path": r.URL.Path, "here": name})
	}))
	t.Cleanup(m.closeDoor)
	return node{m, f}
}

func ids(ms []Member) map[string]bool {
	out := map[string]bool{}
	for _, m := range ms {
		out[m.Name] = true
	}
	return out
}

// Three machines: alpha makes the network and invites beta; beta invites
// gamma. Without anyone linking alpha and gamma, a round later each sees
// the other two, each can reach the others' API, and nothing else.
func TestJoiningOneNodeIsJoiningAll(t *testing.T) {
	ctx := context.Background()
	port := freePort(t)
	a, b, c := newNode(t, "alpha", port), newNode(t, "beta", port), newNode(t, "gamma", port)

	if _, _, err := a.Invite(ctx, ""); err != ErrNoEndpoint {
		t.Fatalf("a network made without an endpoint: %v", err)
	}
	code, _, err := a.Invite(ctx, "198.51.100.1:51821")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Join(ctx, code, "198.51.100.2:51821"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Join(ctx, code, ""); err == nil {
		t.Fatal("an invitation worked twice")
	}
	if c.InNetwork() {
		t.Fatal("a failed join left the node in a network")
	}
	code2, _, err := b.Invite(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Join(ctx, code2, ""); err != nil {
		t.Fatal(err)
	}
	// gamma learnt alpha from beta's roster; alpha learns gamma in a round.
	if !ids(c.Members())["alpha"] {
		t.Fatalf("gamma does not know alpha: %+v", c.Members())
	}
	a.Gossip(ctx)
	for _, n := range []node{a, b, c} {
		if len(n.Members()) != 2 {
			t.Fatalf("%s sees %v", n.Self().Name, ids(n.Members()))
		}
		if len(n.net.peers()) != 2 {
			t.Fatalf("%s has %d tunnel peers", n.Self().Name, len(n.net.peers()))
		}
	}
	// The key that travelled in the code is not the one gamma uses.
	cm, _ := a.Member(c.Self().ID)
	if code2key, _ := decodeCode(code2); code2key.Key.IsZero() || func() bool { pub, _ := code2key.Key.Public(); return pub == cm.PublicKey }() {
		t.Fatal("gamma kept the key from the invitation")
	}
	if a.net.fw.APIPort != port || a.net.fw.Interface != Interface {
		t.Fatalf("firewall %+v", a.net.fw)
	}

	// alpha talks to gamma's API, and gamma knows it was alpha.
	var out map[string]string
	if err := a.Call(ctx, cm, "GET", "/sites", nil, &out); err != nil || out["from"] != "alpha" || out["here"] != "gamma" {
		t.Fatalf("%v %v", err, out)
	}
	// Without the network's secret, the door is shut.
	if err := a.call(ctx, cm.IP, "wrong", "GET", "/sites", nil, &out); err == nil {
		t.Fatal("the door opened without the secret")
	}

	// A rename travels.
	if err := c.SetName("gamma-2"); err != nil {
		t.Fatal(err)
	}
	b.Gossip(ctx)
	if !ids(b.Members())["gamma-2"] {
		t.Fatalf("the rename did not travel: %v", ids(b.Members()))
	}

	// alpha removes gamma: gamma is told and leaves, beta learns it.
	if _, err := a.Expel(ctx, c.Self().ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if c.InNetwork() {
		t.Fatal("gamma was not told")
	}
	b.Gossip(ctx)
	if ids(b.Members())["gamma-2"] || len(b.net.peers()) != 1 {
		t.Fatalf("beta still has gamma: %v", ids(b.Members()))
	}

	// beta leaves on its own.
	if err := b.Leave(ctx); err != nil {
		t.Fatal(err)
	}
	if len(a.Members()) != 0 {
		t.Fatalf("alpha still sees %v", ids(a.Members()))
	}
	if b.InNetwork() || b.net.up {
		t.Fatal("beta is still up")
	}
}

// A removal is newer than the copy a node that was offline brings back.
func TestAnOldRosterDoesNotBringARemovedNodeBack(t *testing.T) {
	m := &Mesh{now: time.Now, seen: map[string]time.Time{}}
	m.self = Self{ID: "self"}
	k, _ := wireguard.GeneratePrivateKey()
	pub, _ := k.Public()
	ip := netip.MustParseAddr("10.66.1.2")
	t0 := time.Now()
	m.st.Members = []Member{{ID: "x", Name: "x", IP: ip, PublicKey: pub, Updated: t0, Removed: true}}
	m.mergeLocked([]Member{{ID: "x", Name: "x", IP: ip, PublicKey: pub, Updated: t0.Add(-time.Hour)}})
	if len(m.liveLocked()) != 0 {
		t.Fatal("an old copy revived a removed node")
	}
	m.st.Updated = t0
	if _, ex := m.mergeLocked([]Member{{ID: "self", IP: ip, PublicKey: pub, Updated: t0.Add(time.Minute), Removed: true}}); !ex {
		t.Fatal("a newer removal of this node was not noticed")
	}
}

// beta's names reach alpha in a round; a connection alpha relays for one
// of them arrives at beta with the device it came from, and the network's
// "panel only through a VPN" set at beta reaches alpha.
func TestNamesRelayAndSettingsTravelTheNetwork(t *testing.T) {
	ctx := context.Background()
	port := freePort(t)
	relay := port
	for relay == port {
		relay = freePort(t)
	}
	a, b := newNode(t, "alpha", port), newNode(t, "beta", port)
	a.opts.RelayPort, b.opts.RelayPort = relay, relay
	b.opts.Names = func() []string { return []string{"b.test", "panel-b.test"} }
	got := make(chan string, 1)
	b.opts.OnRelay = func(c net.Conn, device string) {
		buf := make([]byte, 5)
		_, _ = io.ReadFull(c, buf)
		_, _ = c.Write([]byte("ok:" + device))
		got <- string(buf)
		c.Close()
	}
	var settings []NetSettings
	a.opts.OnSettings = func(s NetSettings) { settings = append(settings, s) }

	code, _, err := a.Invite(ctx, "198.51.100.1:51821")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Join(ctx, code, ""); err != nil {
		t.Fatal(err)
	}
	if err := b.SetSettings(NetSettings{PanelVPNOnly: true}); err != nil {
		t.Fatal(err)
	}
	a.Gossip(ctx)
	if x, ok := a.Owner("www.b.test"); !ok || x.Name != "beta" {
		t.Fatalf("alpha does not know who serves www.b.test: %v %v", x, ok)
	}
	if _, ok := a.Owner("nobody.test"); ok {
		t.Fatal("a name nobody serves has an owner")
	}
	if !a.Settings().PanelVPNOnly || len(settings) == 0 || !settings[len(settings)-1].PanelVPNOnly {
		t.Fatalf("the setting did not travel: %+v %+v", a.Settings(), settings)
	}

	dev, client := net.Pipe()
	go func() { _ = a.Relay(dev, "b.test", []byte("hello"), "phone") }()
	reply := make([]byte, 8)
	if _, err := io.ReadFull(client, reply); err != nil || string(reply) != "ok:phone" {
		t.Fatalf("%q %v", reply, err)
	}
	if first := <-got; first != "hello" {
		t.Fatalf("beta read %q first", first)
	}

	// Not a member, or not the secret: the relay door closes.
	c, err := net.Dial("tcp", netip.AddrPortFrom(b.st.IP, uint16(b.opts.RelayPort)).String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("MWRELAY1 wrong - \nhello"))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, _ := c.Read(make([]byte, 8)); n != 0 {
		t.Fatal("the relay door answered without the secret")
	}
	c.Close()
}
