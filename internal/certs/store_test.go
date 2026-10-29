package certs_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/acme"
	"github.com/dcimorra/the-middleware/internal/acme/acmetest"
	"github.com/dcimorra/the-middleware/internal/certs"
	"github.com/dcimorra/the-middleware/internal/events"
)

// door answers HTTP-01 on a loopback port for the fake CA to knock on.
func door(t *testing.T, solver *acme.HTTP01) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: solver}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

func acmeStore(t *testing.T, dir string, ca *acmetest.Server, allowed ...string) (*certs.Store, *events.Bus) {
	t.Helper()
	solver := acme.NewHTTP01()
	ca.ValidateAddr = door(t, solver)
	issuer := &certs.ACMEIssuer{Dir: dir, Directory: ca.URL, Email: "ops@example.test", HTTP01: solver}
	s := certs.New(dir, issuer)
	s.Allowed = func(d string) bool {
		for _, a := range allowed {
			if a == d {
				return true
			}
		}
		return false
	}
	s.Bus = events.New("")
	return s, s.Bus
}

// serveTLS starts a TLS listener fed by the store and returns its address.
func serveTLS(t *testing.T, s *certs.Store) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{GetCertificate: s.GetCertificate})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = c.(*tls.Conn).Handshake()
				c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

func handshake(t *testing.T, addr, name string, pool *x509.CertPool) *x509.Certificate {
	t.Helper()
	cfg := &tls.Config{ServerName: name, RootCAs: pool}
	if pool == nil {
		cfg.InsecureSkipVerify = true
	}
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, cfg)
	if err != nil {
		t.Fatalf("handshake for %s: %v", name, err)
	}
	defer c.Close()
	return c.ConnectionState().PeerCertificates[0]
}

func typesOf(bus *events.Bus) []string {
	var out []string
	for _, e := range bus.Recent() {
		out = append(out, e.Type)
	}
	return out
}

func TestAFirstHandshakeIssuesOnDemandAndTheNextOneIsServedFromDisk(t *testing.T) {
	dir := t.TempDir()
	ca := acmetest.New(t)
	s, bus := acmeStore(t, dir, ca, "a.test")
	addr := serveTLS(t, s)

	leaf := handshake(t, addr, "a.test", ca.Pool())
	if leaf.DNSNames[0] != "a.test" || ca.Issued() != 1 {
		t.Fatalf("names %v, issued %d", leaf.DNSNames, ca.Issued())
	}
	for _, f := range []string{"key.pem", "chain.pem"} {
		st, err := os.Stat(filepath.Join(dir, "a.test", f))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, err, st)
		}
	}
	if st, _ := os.Stat(filepath.Join(dir, "account.key")); st == nil || st.Mode().Perm() != 0o600 {
		t.Fatal("account key missing or world-readable")
	}
	handshake(t, addr, "a.test", ca.Pool())
	if ca.Issued() != 1 {
		t.Fatalf("second handshake ordered again: %d", ca.Issued())
	}
	if got := typesOf(bus); len(got) != 1 || got[0] != "cert.issued" {
		t.Fatalf("events %v", got)
	}
	// A fresh process over the same folder serves what is on disk.
	again := certs.New(dir, nil)
	info, ok := again.Info("a.test")
	if !ok || time.Until(info.Expires) < 80*24*time.Hour {
		t.Fatalf("not reloaded from disk: %+v %v", info, ok)
	}
	if len(again.List()) != 1 {
		t.Fatalf("list %v", again.List())
	}
}

func TestAStrangerPointingDNSHereGetsTheFallbackAndSpendsNoQuota(t *testing.T) {
	ca := acmetest.New(t)
	s, _ := acmeStore(t, t.TempDir(), ca, "a.test")
	addr := serveTLS(t, s)
	leaf := handshake(t, addr, "stranger.test", nil)
	if leaf.Subject.CommonName != "middleware (no certificate yet)" {
		t.Fatalf("got %q", leaf.Subject.CommonName)
	}
	if ca.Issued() != 0 {
		t.Fatal("an order was placed for a name we do not serve")
	}
	if _, err := s.Obtain(context.Background(), "stranger.test"); !errors.Is(err, certs.ErrNotAllowed) {
		t.Fatalf("want ErrNotAllowed, got %v", err)
	}
	if s.Has("stranger.test") {
		t.Fatal("stranger has a certificate")
	}
}

func TestTheAccountIsRegisteredOnceAndReusedAfterARestart(t *testing.T) {
	dir := t.TempDir()
	ca := acmetest.New(t)
	solver := acme.NewHTTP01()
	ca.ValidateAddr = door(t, solver)
	key, _ := acme.GenerateKey()
	one := &certs.ACMEIssuer{Dir: dir, Directory: ca.URL, HTTP01: solver}
	if _, err := one.Issue(context.Background(), "a.test", key); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(dir, "account.json"))
	two := &certs.ACMEIssuer{Dir: dir, Directory: ca.URL, HTTP01: solver}
	if _, err := two.Issue(context.Background(), "b.test", key); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(dir, "account.json"))
	if string(first) != string(second) || len(first) == 0 {
		t.Fatalf("account changed across restarts:\n%s\n%s", first, second)
	}
}

// fakeIssuer self-signs so tests about the store's own behaviour do not
// need a CA at all.
type fakeIssuer struct {
	calls   atomic.Int32
	fail    error
	block   chan struct{} // when set, Issue waits for it to close
	started chan struct{}
	once    sync.Once
}

func (f *fakeIssuer) Issue(ctx context.Context, domain string, key crypto.Signer) ([]byte, error) {
	f.calls.Add(1)
	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if f.block != nil {
		<-f.block
	}
	if f.fail != nil {
		return nil, f.fail
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: domain}, DNSNames: []string{domain},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(90 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

func fakeStore(t *testing.T, f *fakeIssuer) *certs.Store {
	s := certs.New(t.TempDir(), f)
	s.Allowed = func(string) bool { return true }
	s.Bus = events.New("")
	return s
}

func TestConcurrentFirstRequestsShareOneOrder(t *testing.T) {
	f := &fakeIssuer{block: make(chan struct{}), started: make(chan struct{})}
	s := fakeStore(t, f)
	var wg sync.WaitGroup
	results := make([]*tls.Certificate, 10)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := s.Obtain(context.Background(), "a.test")
			if err != nil {
				t.Error(err)
			}
			results[i] = c
		}(i)
	}
	<-f.started
	time.Sleep(50 * time.Millisecond) // let the other nine queue behind the first
	close(f.block)
	wg.Wait()
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("issuer called %d times", n)
	}
	for _, c := range results[1:] {
		if c != results[0] {
			t.Fatal("waiters got different certificates")
		}
	}
}

func TestAFailedOrderIsNotRetriedForAnHour(t *testing.T) {
	f := &fakeIssuer{fail: errors.New("CA says no")}
	s := fakeStore(t, f)
	if _, err := s.Obtain(context.Background(), "a.test"); err == nil || err.Error() != "CA says no" {
		t.Fatalf("got %v", err)
	}
	_, err := s.Obtain(context.Background(), "a.test")
	if !errors.Is(err, certs.ErrBackoff) {
		t.Fatalf("want ErrBackoff, got %v", err)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("issuer called %d times", f.calls.Load())
	}
	if got := typesOf(s.Bus); len(got) != 1 || got[0] != "cert.failed" {
		t.Fatalf("events %v", got)
	}
	s.Backoff = 0
	if _, err := s.Obtain(context.Background(), "a.test"); !errors.Is(err, certs.ErrBackoff) && err == nil {
		t.Fatal("with no backoff the issuer must be asked again")
	}
	if f.calls.Load() != 2 {
		t.Fatalf("issuer called %d times after backoff ended", f.calls.Load())
	}
}

func TestAHandshakeGetsTheFallbackWhenIssuanceOutlivesItsPatience(t *testing.T) {
	f := &fakeIssuer{block: make(chan struct{})}
	s := fakeStore(t, f)
	s.IssueTimeout = 100 * time.Millisecond
	hello := &tls.ClientHelloInfo{ServerName: "a.test"}
	start := time.Now()
	c, err := s.GetCertificate(hello)
	if err != nil || !s.IsFallback(c) {
		t.Fatalf("want fallback, got %v %v", c, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("handshake waited on the CA")
	}
	close(f.block)
	// The order went on without the handshake; the next one finds it.
	deadline := time.Now().Add(5 * time.Second)
	for !s.Has("a.test") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	c, _ = s.GetCertificate(hello)
	if s.IsFallback(c) || f.calls.Load() != 1 {
		t.Fatalf("fallback %v, calls %d", s.IsFallback(c), f.calls.Load())
	}
}

func TestACertificateExpiringWithinThirtyDaysIsRenewed(t *testing.T) {
	dir := t.TempDir()
	ca := acmetest.New(t)
	ca.Validity = 10 * 24 * time.Hour
	s, bus := acmeStore(t, dir, ca, "a.test", "b.test")
	ctx := context.Background()
	for _, d := range []string{"a.test", "b.test"} {
		if _, err := s.Obtain(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	// b.test stops being served: it must be left to expire.
	s.Allowed = func(d string) bool { return d == "a.test" }
	if due := s.Due(time.Now()); len(due) != 1 || due[0] != "a.test" {
		t.Fatalf("due %v", due)
	}
	ca.Validity = 90 * 24 * time.Hour
	s.RenewDue(ctx)
	if ca.Issued() != 3 {
		t.Fatalf("issued %d, want 3", ca.Issued())
	}
	info, _ := s.Info("a.test")
	if time.Until(info.Expires) < 80*24*time.Hour {
		t.Fatalf("still the old certificate: %v", info.Expires)
	}
	if len(s.Due(time.Now())) != 0 {
		t.Fatal("renewed certificate still due")
	}
	got := typesOf(bus)
	if got[len(got)-1] != "cert.renewed" {
		t.Fatalf("events %v", got)
	}
	// What the listener serves is the new one, not the cached old one.
	addr := serveTLS(t, s)
	if leaf := handshake(t, addr, "a.test", ca.Pool()); time.Until(leaf.NotAfter) < 80*24*time.Hour {
		t.Fatal("listener still serves the old certificate")
	}
}

func TestARateLimitedCAIsReportedAsSuchAndBackedOff(t *testing.T) {
	ca := acmetest.New(t)
	s, bus := acmeStore(t, t.TempDir(), ca, "a.test")
	ca.SetRateLimited(true)
	_, err := s.Obtain(context.Background(), "a.test")
	var p *acme.Problem
	if !errors.As(err, &p) || !p.RateLimited() {
		t.Fatalf("want a rate-limit problem, got %v", err)
	}
	ev := bus.Recent()
	if len(ev) != 1 || ev[0].Type != "cert.failed" || ev[0].Level != "error" {
		t.Fatalf("events %+v", ev)
	}
}

// A name that does not point here yet (a new site on the ports door, a
// domain still on the tunnel) gets its certificate through a TXT record.
func TestDNS01IsPreferredForTheNamesItCoversAndCleansUp(t *testing.T) {
	ca := acmetest.New(t)
	var mu sync.Mutex
	txt := map[string][]string{}
	ca.TXT = func(fqdn string) []string { mu.Lock(); defer mu.Unlock(); return txt[fqdn] }
	// HTTP-01 would fail: nothing listens where the CA knocks.
	ca.ValidateAddr = "127.0.0.1:1"
	solver := acme.NewHTTP01()
	iss := &certs.ACMEIssuer{Dir: t.TempDir(), Directory: ca.URL, HTTP01: solver}
	iss.SetDNS01(&acme.DNS01{
		Covers: func(d string) bool { return strings.HasSuffix(d, ".covered.test") },
		Set: func(_ context.Context, fqdn, value string) (func(context.Context) error, error) {
			mu.Lock()
			txt[fqdn] = append(txt[fqdn], value)
			mu.Unlock()
			return func(context.Context) error { mu.Lock(); delete(txt, fqdn); mu.Unlock(); return nil }, nil
		},
		Wait: func(context.Context, string, string) error { return nil },
	})
	key, _ := acme.GenerateKey()
	if _, err := iss.Issue(context.Background(), "a.covered.test", key); err != nil {
		t.Fatal(err)
	}
	if ca.DNSSolved() != 1 {
		t.Fatalf("dns-01 validations: %d", ca.DNSSolved())
	}
	mu.Lock()
	left := len(txt)
	mu.Unlock()
	if left != 0 {
		t.Fatalf("the TXT record was left behind: %v", txt)
	}
	// A name outside the provider's zones falls back to http-01, which
	// fails here: the point is that dns-01 was not attempted for it.
	if _, err := iss.Issue(context.Background(), "b.elsewhere.test", key); err == nil {
		t.Fatal("http-01 to nowhere succeeded")
	}
	if ca.DNSSolved() != 1 {
		t.Fatal("dns-01 was tried for a name the provider does not cover")
	}
}
