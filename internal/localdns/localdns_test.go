package localdns_test

import (
	"context"
	"encoding/binary"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/localdns"
)

func newServer(t *testing.T, sites ...string) *localdns.Server {
	t.Helper()
	s := &localdns.Server{Serves: func(name string) bool {
		for _, d := range sites {
			if d == name {
				return true
			}
		}
		return false
	}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := s.Listen(ctx, "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAServedNameResolvesToLoopbackAndTheRestIsNXDomain(t *testing.T) {
	s := newServer(t, "misite.middleware", "panel.middleware")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ips, err := localdns.Query(ctx, s.Addr(), "misite.middleware", false)
	if err != nil || len(ips) != 1 || !ips[0].Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("A misite.middleware = %v, %v", ips, err)
	}
	ips, err = localdns.Query(ctx, s.Addr(), "MiSite.Middleware.", true)
	if err != nil || len(ips) != 1 || !ips[0].Equal(net.IPv6loopback) {
		t.Fatalf("AAAA (mixed case, trailing dot) = %v, %v", ips, err)
	}
	// A subdomain of a served name is ours: the proxy decides what to do
	// with it, the resolver must not stand in the way.
	ips, err = localdns.Query(ctx, s.Addr(), "api.misite.middleware", false)
	if err != nil || len(ips) != 1 {
		t.Fatalf("subdomain = %v, %v", ips, err)
	}
	ips, err = localdns.Query(ctx, s.Addr(), "example.com", false)
	if err != nil || ips != nil {
		t.Fatalf("example.com must be NXDOMAIN, got %v, %v", ips, err)
	}
}

func TestTCPAnswersTheSameAsUDP(t *testing.T) {
	s := newServer(t, "misite.middleware")
	c, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	q := []byte{0xAB, 0xCD, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0, 6, 'm', 'i', 's', 'i', 't', 'e', 10, 'm', 'i', 'd', 'd', 'l', 'e', 'w', 'a', 'r', 'e', 0, 0, 1, 0, 1}
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(q)))
	c.Write(append(l[:], q...))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil || n < 2 {
		t.Fatal(err)
	}
	resp := buf[2:n]
	if resp[0] != 0xAB || resp[1] != 0xCD || resp[3]&0xF != 0 || binary.BigEndian.Uint16(resp[6:8]) != 1 {
		t.Fatalf("bad TCP answer: % x", resp)
	}
	if got := net.IP(resp[len(resp)-4:]); !got.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("rdata = %v", got)
	}
}

func TestGarbageAndResponsesAreNotAnswered(t *testing.T) {
	s := &localdns.Server{Serves: func(string) bool { return true }}
	if s.Answer([]byte{1, 2, 3}) != nil {
		t.Fatal("a short packet must get silence")
	}
	resp := []byte{0, 1, 0x81, 0x80, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 1}
	if s.Answer(resp) != nil {
		t.Fatal("a response must not be answered (reflection)")
	}
	// A compression pointer in the question is refused rather than
	// followed, so a crafted packet cannot loop the parser.
	loop := []byte{0, 1, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0, 0xC0, 0x0C, 0, 1, 0, 1}
	out := s.Answer(loop)
	if out == nil || out[3]&0xF != 1 {
		t.Fatalf("a pointer in the question must be FORMERR, got % x", out)
	}
}

// TestGoResolverAgrees asks through net.Resolver, a DNS client we did not
// write, so the wire format is checked against an independent parser
// (both UDP and, when it retries, TCP).
func TestGoResolverAgrees(t *testing.T) {
	s := newServer(t, "misite.middleware")
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, s.Addr())
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := r.LookupIPAddr(ctx, "misite.middleware")
	if err != nil {
		t.Fatal(err)
	}
	var v4, v6 bool
	for _, ip := range ips {
		if ip.IP.Equal(net.IPv4(127, 0, 0, 1)) {
			v4 = true
		}
		if ip.IP.Equal(net.IPv6loopback) {
			v6 = true
		}
	}
	if !v4 || !v6 {
		t.Fatalf("resolver got %v", ips)
	}
	if _, err := r.LookupIPAddr(ctx, "nothere.example"); err == nil {
		t.Fatal("an unknown name must fail to resolve")
	} else if dnsErr, ok := err.(*net.DNSError); !ok || !dnsErr.IsNotFound {
		t.Fatalf("want not-found, got %v", err)
	}
}

// TestTheSystemResolverAgrees asks the real dig, when the box has one.
func TestTheSystemResolverAgrees(t *testing.T) {
	dig, err := exec.LookPath("dig")
	if err != nil {
		t.Skip("dig not installed")
	}
	s := newServer(t, "misite.middleware")
	host, port, _ := net.SplitHostPort(s.Addr())
	out, err := exec.Command(dig, "+short", "+time=2", "+tries=1", "@"+host, "-p", port, "misite.middleware", "A").CombinedOutput()
	if err != nil {
		t.Fatalf("dig: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "127.0.0.1" {
		t.Fatalf("dig said %q", out)
	}
}
