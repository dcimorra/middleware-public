package helper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/wireguard"
)

// testHelper is a real server on a real unix socket, with the system
// behind a recorder and the peer check stubbed to whatever the test says.
type testHelper struct {
	srv *Server
	rec *Recorder
	cli *Client
	dir string

	mu   sync.Mutex
	peer Peer
}

func (h *testHelper) setPeer(p Peer) {
	h.mu.Lock()
	h.peer = p
	h.mu.Unlock()
}

func newTestHelper(t *testing.T, configure ...func(*Server)) *testHelper {
	t.Helper()
	// t.TempDir() paths can exceed the 108-byte limit of a unix socket path.
	dir, err := os.MkdirTemp("", "mwh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	h := &testHelper{rec: &Recorder{Outputs: map[string]string{}, Fail: map[string]error{}}, dir: dir, peer: Peer{UID: 1000, GID: 1000}}
	h.srv = &Server{
		Socket:      filepath.Join(dir, "h.sock"),
		SocketGroup: -1,
		AllowUID:    1000,
		AllowGID:    -1,
		Exec:        h.rec,
		Credentials: func(net.Conn) (Peer, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.peer, nil
		},
		WireguardDir: filepath.Join(dir, "wireguard"),
		SysctlDir:    filepath.Join(dir, "sysctl.d"),
		SysNetDir:    filepath.Join(dir, "net"),
		Log:          log.New(io.Discard, "", 0),
	}
	for _, c := range configure {
		c(h.srv)
	}
	ln, err := h.srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = h.srv.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })
	h.cli = Dial(h.srv.Socket)
	return h
}

func (h *testHelper) ifaceUp(name string) {
	os.MkdirAll(filepath.Join(h.srv.SysNetDir, name), 0o755)
}

func TestARequestTravelsTheSocketAndComesBackWithItsID(t *testing.T) {
	h := newTestHelper(t, func(s *Server) { s.Build = "test-build" })
	if !h.cli.Available() {
		t.Fatal("helper should be available")
	}
	r, err := h.cli.Ping(context.Background())
	if err != nil || r.Version != Version || r.Build != "test-build" {
		t.Fatalf("ping: %+v %v", r, err)
	}
	// Raw: the id must come back and an unknown op must be refused.
	conn, err := net.Dial("unix", h.srv.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, `{"id":"abc","op":"shell.exec","args":{"cmd":"rm -rf /"}}`+"\n")
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != "abc" || resp.OK || !strings.Contains(resp.Error, "unknown operation") {
		t.Fatalf("got %+v", resp)
	}
	if len(h.rec.Calls) != 0 {
		t.Fatalf("something was executed: %v", h.rec.Lines())
	}
}

func TestAPeerWithTheWrongUIDIsRefusedBeforeAnyOperation(t *testing.T) {
	h := newTestHelper(t)
	h.setPeer(Peer{UID: 1001, GID: 1001})
	_, err := h.cli.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected refusal, got %v", err)
	}
	h.setPeer(Peer{UID: 0})
	if _, err := h.cli.Ping(context.Background()); err != nil {
		t.Fatalf("root always passes: %v", err)
	}
	byGroup := newTestHelper(t, func(s *Server) {
		s.AllowUID, s.AllowGID = -1, 1000
		s.GroupsOf = func(uid uint32) []int { return []int{int(uid) + 1} }
	})
	byGroup.setPeer(Peer{UID: 1001, GID: 1000})
	if _, err := byGroup.cli.Ping(context.Background()); err != nil {
		t.Fatalf("gid match should pass: %v", err)
	}
	byGroup.setPeer(Peer{UID: 1000, GID: 1001})
	if _, err := byGroup.cli.Ping(context.Background()); err == nil {
		t.Fatal("uid is not allowed when only the gid is configured")
	}
}

func TestAMissingSocketIsNotAvailableAndCallsSayUnavailable(t *testing.T) {
	c := Dial(filepath.Join(t.TempDir(), "nope.sock"))
	if c.Available() {
		t.Fatal("available without a socket")
	}
	if _, err := c.Ping(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestFirewallApplyLoadsTheRenderedRulesetThroughNft(t *testing.T) {
	h := newTestHelper(t)
	err := h.cli.FirewallApply(context.Background(), FirewallPolicy{
		AllowPorts: []int{443, 80, 51820, 80}, VPNOnly: true, VPNInterface: "mw0", PanelPorts: []int{46100, 22},
	})
	if err != nil {
		t.Fatal(err)
	}
	last, _ := h.rec.Last()
	if last.Line() != "nft -f -" {
		t.Fatalf("got %q", last.Line())
	}
	want := `table inet middleware {
	set blocked4 {
		type ipv4_addr
		flags timeout
	}
	set blocked6 {
		type ipv6_addr
		flags timeout
	}
	chain input {
		type filter hook input priority -10; policy accept;
	}
}
flush chain inet middleware input
table inet middleware {
	chain input {
		ip saddr @blocked4 drop
		ip6 saddr @blocked6 drop
		iifname "lo" accept
		tcp dport { 80, 443, 51820 } accept
		udp dport { 80, 443, 51820 } accept
		iifname "mw0" tcp dport { 22, 46100 } accept
		tcp dport { 22, 46100 } drop
	}
}
`
	if last.Stdin != want {
		t.Fatalf("ruleset:\n%s\nwant:\n%s", last.Stdin, want)
	}
	// Without vpn_only the panel lines are absent and nothing else changes.
	h.rec.Reset()
	if err := h.cli.FirewallApply(context.Background(), FirewallPolicy{}); err != nil {
		t.Fatal(err)
	}
	last, _ = h.rec.Last()
	if strings.Contains(last.Stdin, "dport") {
		t.Fatalf("ports leaked into an empty policy:\n%s", last.Stdin)
	}
}

func TestFirewallApplyRefusesBadInputBeforeRunningAnything(t *testing.T) {
	h := newTestHelper(t)
	bad := []FirewallPolicy{
		{AllowPorts: []int{0}},
		{AllowPorts: []int{70000}},
		{VPNOnly: true, VPNInterface: "mw0; nft flush ruleset", PanelPorts: []int{46100}},
		{VPNOnly: true, VPNInterface: "mw0"},
		{VPNOnly: true, VPNInterface: "", PanelPorts: []int{46100}},
	}
	for i, p := range bad {
		if err := h.cli.FirewallApply(context.Background(), p); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	if len(h.rec.Calls) != 0 {
		t.Fatalf("executed: %v", h.rec.Lines())
	}
	// An unknown field is a protocol mismatch, not something to ignore.
	resp := h.srv.Handle(context.Background(), Peer{}, Request{Op: OpFirewallApply, Args: json.RawMessage(`{"vpn_onli": true}`)})
	if resp.OK {
		t.Fatal("unknown field accepted")
	}
}

func TestFirewallBlockAddsATimedElementAndUnblockDeletesIt(t *testing.T) {
	h := newTestHelper(t)
	if err := h.cli.FirewallBlock(context.Background(), "203.0.113.9", 10*time.Minute, "rate limit"); err != nil {
		t.Fatal(err)
	}
	lines := h.rec.Lines()
	want := []string{
		"nft delete element inet middleware blocked4 { 203.0.113.9 }",
		"nft add element inet middleware blocked4 { 203.0.113.9 timeout 600s }",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q", lines)
	}
	h.rec.Reset()
	if err := h.cli.FirewallBlock(context.Background(), "2001:db8::1", time.Second, ""); err != nil {
		t.Fatal(err)
	}
	last, _ := h.rec.Last()
	if last.Line() != "nft add element inet middleware blocked6 { 2001:db8::1 timeout 1s }" {
		t.Fatalf("got %q", last.Line())
	}
	h.rec.Reset()
	if err := h.cli.FirewallUnblock(context.Background(), "::ffff:203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	last, _ = h.rec.Last()
	if last.Line() != "nft delete element inet middleware blocked4 { 203.0.113.9 }" {
		t.Fatalf("mapped v4 should land in the v4 set: %q", last.Line())
	}
	h.rec.Reset()
	for _, ip := range []string{"", "203.0.113.0/24", "fe80::1%eth0", "1.2.3.4 }; flush ruleset; {"} {
		if err := h.cli.FirewallBlock(context.Background(), ip, time.Minute, ""); err == nil {
			t.Errorf("%q accepted", ip)
		}
	}
	if err := h.cli.FirewallBlock(context.Background(), "1.2.3.4", 400*24*time.Hour, ""); err == nil {
		t.Error("a block longer than a year was accepted")
	}
	if len(h.rec.Calls) != 0 {
		t.Fatalf("executed: %v", h.rec.Lines())
	}
}

func TestFirewallStatusReadsTheSetsAsNftPrintsThem(t *testing.T) {
	h := newTestHelper(t)
	h.rec.Outputs["nft -j list set inet middleware blocked4"] = `{"nftables": [{"metainfo": {"version": "1.0.9", "release_name": "Old Doc Yak #3", "json_schema_version": 1}}, {"set": {"family": "inet", "name": "blocked4", "table": "middleware", "type": "ipv4_addr", "handle": 2, "flags": ["timeout"], "elem": [{"elem": {"val": "203.0.113.9", "timeout": 600, "expires": 512}}, "198.51.100.1"]}}]}`
	h.rec.Fail["nft -j list set inet middleware blocked6"] = errors.New("nft: Error: No such file or directory")
	st, err := h.cli.FirewallStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Blocked) != 2 || st.Blocked[0].IP != "203.0.113.9" || st.Blocked[0].Expires != 512*time.Second || st.Blocked[1].IP != "198.51.100.1" || st.Blocked[1].Expires != 0 {
		t.Fatalf("got %+v", st.Blocked)
	}
}

func serverConfig(t *testing.T) wireguard.ServerConfig {
	t.Helper()
	priv, _ := wireguard.GeneratePrivateKey()
	peer, _ := wireguard.GeneratePrivateKey()
	pub, _ := peer.Public()
	return wireguard.ServerConfig{
		Interface: "mw0", PrivateKey: priv, Address: netip.MustParsePrefix("10.77.0.1/24"), ListenPort: 51820,
		Peers: []wireguard.Peer{{Name: "laptop", PublicKey: pub, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("10.77.0.2/32")}}},
	}
}

func TestVPNApplyWritesTheConfigOwnerOnlyAndSyncsOnlyWhenUp(t *testing.T) {
	h := newTestHelper(t)
	cfg := serverConfig(t)
	if err := h.cli.VPNApply(context.Background(), VPNApplyArgs{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.srv.WireguardDir, "mw0.conf")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	want, _ := wireguard.RenderServer(cfg)
	if string(data) != want {
		t.Fatalf("file:\n%s", data)
	}
	if len(h.rec.Calls) != 0 {
		t.Fatalf("interface is down, nothing should run: %v", h.rec.Lines())
	}
	h.ifaceUp("mw0")
	h.rec.Outputs["wg-quick strip "+path] = "[Interface]\nPrivateKey = x\n"
	if err := h.cli.VPNApply(context.Background(), VPNApplyArgs{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	lines := h.rec.Lines()
	if len(lines) != 2 || lines[0] != "wg-quick strip "+path || lines[1] != "wg syncconf mw0 /dev/stdin" {
		t.Fatalf("got %q", lines)
	}
	if h.rec.Calls[1].Stdin != "[Interface]\nPrivateKey = x\n" {
		t.Fatalf("syncconf did not get the stripped file: %q", h.rec.Calls[1].Stdin)
	}
	// Invalid config: nothing written, nothing run.
	h.rec.Reset()
	bad := cfg
	bad.Interface = "../../etc/passwd"
	if err := h.cli.VPNApply(context.Background(), VPNApplyArgs{Config: bad}); err == nil {
		t.Fatal("path traversal accepted")
	}
	if entries, _ := os.ReadDir(h.srv.WireguardDir); len(entries) != 1 || len(h.rec.Calls) != 0 {
		t.Fatalf("side effects: %v %v", entries, h.rec.Lines())
	}
}

func TestVPNUpAndDownUseWgQuickAndAreIdempotent(t *testing.T) {
	h := newTestHelper(t)
	if err := h.cli.VPNUp(context.Background(), "mw0"); err == nil {
		t.Fatal("up without a config should fail")
	}
	if err := h.cli.VPNApply(context.Background(), VPNApplyArgs{Config: serverConfig(t)}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.srv.WireguardDir, "mw0.conf")
	if err := h.cli.VPNUp(context.Background(), "mw0"); err != nil {
		t.Fatal(err)
	}
	if last, _ := h.rec.Last(); last.Line() != "wg-quick up "+path {
		t.Fatalf("got %q", last.Line())
	}
	h.rec.Reset()
	h.ifaceUp("mw0")
	if err := h.cli.VPNUp(context.Background(), "mw0"); err != nil || len(h.rec.Calls) != 0 {
		t.Fatalf("up on an up interface should be a no-op: %v %v", err, h.rec.Lines())
	}
	if err := h.cli.VPNDown(context.Background(), "mw0"); err != nil {
		t.Fatal(err)
	}
	if last, _ := h.rec.Last(); last.Line() != "wg-quick down "+path {
		t.Fatalf("got %q", last.Line())
	}
	h.rec.Fail["wg-quick down "+path] = errors.New("wg-quick: `mw0' is not a WireGuard interface")
	if err := h.cli.VPNDown(context.Background(), "mw0"); err == nil || !strings.Contains(err.Error(), "not a WireGuard interface") {
		t.Fatalf("the binary's error must reach the client: %v", err)
	}
	if err := h.cli.VPNUp(context.Background(), "Mw0"); err == nil {
		t.Fatal("bad interface name accepted")
	}
}

func TestVPNStatusParsesWgShowDump(t *testing.T) {
	h := newTestHelper(t)
	st, err := h.cli.VPNStatus(context.Background(), "mw0")
	if err != nil || st.Up || len(st.Peers) != 0 {
		t.Fatalf("down interface: %+v %v", st, err)
	}
	h.ifaceUp("mw0")
	cfg := serverConfig(t)
	pub, _ := cfg.PrivateKey.Public()
	h.rec.Outputs["wg show mw0 dump"] = cfg.PrivateKey.String() + "\t" + pub.String() + "\t51820\toff\n" +
		cfg.Peers[0].PublicKey.String() + "\t(none)\t203.0.113.5:40000\t10.77.0.2/32\t1760000000\t10\t20\toff\n"
	st, err = h.cli.VPNStatus(context.Background(), "mw0")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Up || st.PublicKey != pub || st.ListenPort != 51820 || len(st.Peers) != 1 || st.Peers[0].LastHandshake.Unix() != 1760000000 {
		t.Fatalf("got %+v", st)
	}
}

func TestReservePortsWritesTheSysctlDropInAndAppliesIt(t *testing.T) {
	h := newTestHelper(t)
	if err := h.cli.ReservePorts(context.Background(), 33000, 47000); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.srv.SysctlDir, ReservedPortsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "net.ipv4.ip_local_reserved_ports = 33000-47000\n") {
		t.Fatalf("file:\n%s", data)
	}
	if last, _ := h.rec.Last(); last.Line() != "sysctl -p "+path {
		t.Fatalf("got %q", last.Line())
	}
	for _, r := range [][2]int{{47000, 33000}, {80, 1000}, {1024, 70000}} {
		if err := h.cli.ReservePorts(context.Background(), r[0], r[1]); err == nil {
			t.Errorf("%v accepted", r)
		}
	}
}

func TestEveryListedOperationHasAHandler(t *testing.T) {
	s := &Server{AllowUID: -1, AllowGID: -1, SocketGroup: -1}
	s.init()
	for _, op := range Ops {
		if _, ok := s.ops[op]; !ok {
			t.Errorf("%s has no handler", op)
		}
	}
	if len(s.ops) != len(Ops) {
		t.Fatalf("%d handlers for %d listed ops", len(s.ops), len(Ops))
	}
}

// Set NFT=1 to check the rendered ruleset against the real nft parser
// (`nft -c` checks syntax without loading; it still needs CAP_NET_ADMIN on
// most kernels, so this is a root-only test).
func TestTheRulesetParsesWithRealNft(t *testing.T) {
	if os.Getenv("NFT") == "" {
		t.Skip("set NFT=1 to run the ruleset through nft -c")
	}
	script, _ := RenderRuleset(FirewallPolicy{AllowPorts: []int{443}, VPNOnly: true, VPNInterface: "mw0", PanelPorts: []int{46100}})
	e := &SystemExec{}
	if _, err := e.Run(context.Background(), "nft", []string{"-c", "-f", "-"}, script); err != nil {
		t.Fatal(err)
	}
}

// The operator's shell has the group as a secondary one; SO_PEERCRED says
// only the primary. Membership is read from the system, by uid.
func TestAMemberOfTheGroupAsASecondaryOneIsAllowed(t *testing.T) {
	s := &Server{AllowUID: -1, AllowGID: 993, GroupsOf: func(uid uint32) []int {
		if uid == 1000 {
			return []int{1000, 993}
		}
		return []int{int(uid)}
	}}
	if !s.allowed(Peer{UID: 1000, GID: 1000}) {
		t.Fatal("a member by a secondary group was refused")
	}
	if s.allowed(Peer{UID: 1001, GID: 1001}) {
		t.Fatal("a stranger was let in")
	}
}
