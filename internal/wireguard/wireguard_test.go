package wireguard

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestAGeneratedPrivateKeyIsClampedAndBase64With44Chars(t *testing.T) {
	k, err := GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	s := k.String()
	if len(s) != 44 || !strings.HasSuffix(s, "=") {
		t.Fatalf("expected 44 chars of padded base64, got %q", s)
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) != 32 {
		t.Fatalf("decode: %v, %d bytes", err, len(raw))
	}
	if raw[0]&7 != 0 || raw[31]&128 != 0 || raw[31]&64 == 0 {
		t.Fatalf("key is not clamped like wg genkey: %x", raw)
	}
	back, err := ParseKey(s)
	if err != nil || back != k {
		t.Fatalf("round trip lost the key: %v", err)
	}
}

// The public key must be what `wg pubkey` prints. RFC 7748 §6.1 gives Alice's
// scalar and public key; wg uses the same X25519 with the same base point.
func TestThePublicKeyMatchesTheRFC7748Vector(t *testing.T) {
	priv, _ := hex.DecodeString("77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a")
	var k Key
	copy(k[:], priv)
	pub, err := k.Public()
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(pub[:]); got != "8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a" {
		t.Fatalf("got %s", got)
	}
}

// Set WG=1 to compare against the real wireguard-tools on this machine.
func TestThePublicKeyMatchesWgPubkeyWhenAvailable(t *testing.T) {
	if os.Getenv("WG") == "" {
		t.Skip("set WG=1 to compare with the wg binary")
	}
	if _, err := exec.LookPath("wg"); err != nil {
		t.Skip("wg not installed")
	}
	k, _ := GeneratePrivateKey()
	cmd := exec.Command("wg", "pubkey")
	cmd.Stdin = strings.NewReader(k.String() + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := k.Public()
	if strings.TrimSpace(string(out)) != pub.String() {
		t.Fatalf("wg says %s, we say %s", out, pub)
	}
}

func TestAKeyIsBase64InJSON(t *testing.T) {
	k, _ := GeneratePresharedKey()
	data, _ := json.Marshal(struct {
		K Key `json:"k"`
		Z Key `json:"z"`
	}{K: k})
	if !strings.Contains(string(data), `"k":"`+k.String()+`"`) {
		t.Fatalf("got %s", data)
	}
	var back struct {
		K Key `json:"k"`
		Z Key `json:"z"`
	}
	if err := json.Unmarshal(data, &back); err != nil || back.K != k || !back.Z.IsZero() {
		t.Fatalf("round trip: %v %v", err, back)
	}
	if _, err := ParseKey("not a key"); err == nil {
		t.Fatal("garbage was accepted")
	}
}

func TestTheServerFileHasOneInterfaceAndOnePeerSectionPerClient(t *testing.T) {
	priv, _ := GeneratePrivateKey()
	p1, _ := GeneratePrivateKey()
	pub1, _ := p1.Public()
	psk, _ := GeneratePresharedKey()
	p2, _ := GeneratePrivateKey()
	pub2, _ := p2.Public()
	text, err := RenderServer(ServerConfig{
		Interface: "mw0", PrivateKey: priv, Address: netip.MustParsePrefix("10.77.0.1/24"), ListenPort: 51820,
		Peers: []Peer{
			{Name: "laptop", PublicKey: pub1, PresharedKey: psk, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("10.77.0.2/32")}},
			{Name: "phone", PublicKey: pub2, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("10.77.0.3/32")}, PersistentKeepalive: 25},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "[Interface]\nPrivateKey = " + priv.String() + "\nAddress = 10.77.0.1/24\nListenPort = 51820\n\n[Peer]\n# laptop\nPublicKey = " + pub1.String() + "\nPresharedKey = " + psk.String() + "\nAllowedIPs = 10.77.0.2/32\n\n[Peer]\n# phone\nPublicKey = " + pub2.String() + "\nAllowedIPs = 10.77.0.3/32\nPersistentKeepalive = 25\n"
	if !strings.HasSuffix(text, want) {
		t.Fatalf("got:\n%s\nwant suffix:\n%s", text, want)
	}
	if strings.Count(text, "[Peer]") != 2 {
		t.Fatalf("expected two peers:\n%s", text)
	}
}

func TestTheClientFileHoldsItsPrivateKeyAndTheServerEndpoint(t *testing.T) {
	priv, _ := GeneratePrivateKey()
	srv, _ := GeneratePrivateKey()
	srvPub, _ := srv.Public()
	text, err := RenderClient(ClientConfig{
		PrivateKey: priv, Address: netip.MustParsePrefix("10.77.0.2/32"), DNS: "10.77.0.1",
		ServerPublicKey: srvPub, Endpoint: "vpn.example.com:51820",
		AllowedIPs: []netip.Prefix{netip.MustParsePrefix("10.77.0.0/24")}, PersistentKeepalive: 25,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "[Interface]\nPrivateKey = " + priv.String() + "\nAddress = 10.77.0.2/32\nDNS = 10.77.0.1\n\n[Peer]\nPublicKey = " + srvPub.String() + "\nEndpoint = vpn.example.com:51820\nAllowedIPs = 10.77.0.0/24\nPersistentKeepalive = 25\n"
	if text != want {
		t.Fatalf("got:\n%s\nwant:\n%s", text, want)
	}
}

func TestAConfigWithABadInterfaceEndpointOrPortIsRejected(t *testing.T) {
	priv, _ := GeneratePrivateKey()
	pub, _ := priv.Public()
	good := ServerConfig{Interface: "mw0", PrivateKey: priv, Address: netip.MustParsePrefix("10.77.0.1/24"), ListenPort: 51820,
		Peers: []Peer{{PublicKey: pub, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("10.77.0.2/32")}}}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []func(c *ServerConfig){
		func(c *ServerConfig) { c.Interface = "wg-0; rm -rf /" },
		func(c *ServerConfig) { c.Interface = "" },
		func(c *ServerConfig) { c.ListenPort = 70000 },
		func(c *ServerConfig) { c.PrivateKey = Key{} },
		func(c *ServerConfig) { c.Peers[0].AllowedIPs = nil },
		func(c *ServerConfig) { c.Peers[0].Endpoint = "host:1\nPrivateKey = x" },
		func(c *ServerConfig) { c.Peers[0].Name = "Bad Name" },
		func(c *ServerConfig) { c.Peers = append(c.Peers, c.Peers[0]) },
	}
	for i, mutate := range bad {
		c := good
		c.Peers = append([]Peer(nil), good.Peers...)
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("case %d was accepted", i)
		}
	}
}

func TestAddressesAreAllocatedFromTheLowestFreeHost(t *testing.T) {
	subnet := netip.MustParsePrefix("10.77.0.0/24")
	if got := ServerAddress(subnet); got != netip.MustParseAddr("10.77.0.1") {
		t.Fatalf("server address %s", got)
	}
	a, err := NextAddress(subnet, nil)
	if err != nil || a != netip.MustParseAddr("10.77.0.2") {
		t.Fatalf("first: %s %v", a, err)
	}
	a, _ = NextAddress(subnet, []netip.Addr{netip.MustParseAddr("10.77.0.2"), netip.MustParseAddr("10.77.0.4")})
	if a != netip.MustParseAddr("10.77.0.3") {
		t.Fatalf("gap not reused: %s", a)
	}
	// A /30 has one host left after the server: .2; .3 is broadcast.
	small := netip.MustParsePrefix("10.77.0.0/30")
	a, err = NextAddress(small, nil)
	if err != nil || a != netip.MustParseAddr("10.77.0.2") {
		t.Fatalf("/30: %s %v", a, err)
	}
	if _, err = NextAddress(small, []netip.Addr{a}); err != ErrSubnetFull {
		t.Fatalf("expected full, got %v", err)
	}
}

func TestWgShowDumpIsParsedIntoPeersWithHandshakes(t *testing.T) {
	priv, _ := GeneratePrivateKey()
	pub, _ := priv.Public()
	peer, _ := GeneratePrivateKey()
	peerPub, _ := peer.Public()
	text := priv.String() + "\t" + pub.String() + "\t51820\toff\n" +
		peerPub.String() + "\t(none)\t203.0.113.5:40000\t10.77.0.2/32\t1760000000\t1234\t5678\t25\n" +
		peerPub.String() + "\t(none)\t(none)\t10.77.0.3/32\t0\t0\t0\toff\n"
	d, err := ParseDump(text)
	if err != nil {
		t.Fatal(err)
	}
	if d.PublicKey != pub || d.ListenPort != 51820 || len(d.Peers) != 2 {
		t.Fatalf("got %+v", d)
	}
	p := d.Peers[0]
	if p.Endpoint != "203.0.113.5:40000" || p.AllowedIPs[0] != "10.77.0.2/32" || p.LastHandshake.Unix() != 1760000000 || p.ReceiveBytes != 1234 || p.TransmitBytes != 5678 || p.PersistentKeepalive != 25 {
		t.Fatalf("peer 0: %+v", p)
	}
	if !p.Online(time.Unix(1760000060, 0)) || p.Online(time.Unix(1760001000, 0)) {
		t.Fatal("online window is wrong")
	}
	if q := d.Peers[1]; q.Endpoint != "" || !q.LastHandshake.IsZero() || q.PersistentKeepalive != 0 {
		t.Fatalf("peer 1: %+v", q)
	}
	if empty, err := ParseDump(""); err != nil || len(empty.Peers) != 0 {
		t.Fatalf("empty dump: %v", err)
	}
}
