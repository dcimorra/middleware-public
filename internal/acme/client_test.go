package acme_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/acme"
	"github.com/dcimorra/the-middleware/internal/acme/acmetest"
)

func newClient(t *testing.T, ca *acmetest.Server) *acme.Client {
	t.Helper()
	key, err := acme.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return &acme.Client{Directory: ca.URL, Key: key, UserAgent: "middleware-test"}
}

// door starts a loopback listener that answers HTTP-01 from solver and
// returns its address, so the fake CA has something real to knock on.
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

func TestAnAccountIsCreatedOnceAndFoundAgainWithTheSameKey(t *testing.T) {
	ca := acmetest.New(t)
	c := newClient(t, ca)
	ctx := context.Background()
	a, err := c.Register(ctx, "mailto:ops@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if a.URL == "" || c.KID != a.URL {
		t.Fatalf("account %+v, kid %q", a, c.KID)
	}
	again := &acme.Client{Directory: ca.URL, Key: c.Key}
	b, err := again.Register(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if b.URL != a.URL {
		t.Fatalf("second registration found %q, want %q", b.URL, a.URL)
	}
}

func TestAStaleNonceIsRetriedWithTheOneTheCASendsBack(t *testing.T) {
	ca := acmetest.New(t)
	c := newClient(t, ca)
	ca.RejectNextNonce(2)
	if _, err := c.Register(context.Background()); err != nil {
		t.Fatalf("two badNonce answers must be absorbed: %v", err)
	}
	// Beyond the retry budget the error is surfaced, not hidden.
	ca.RejectNextNonce(10)
	_, err := c.NewOrder(context.Background(), []string{"a.test"})
	var p *acme.Problem
	if !errors.As(err, &p) || !p.IsCode("badNonce") {
		t.Fatalf("want badNonce after exhausting retries, got %v", err)
	}
}

func TestARateLimitIsATypedErrorWithRetryAfter(t *testing.T) {
	ca := acmetest.New(t)
	c := newClient(t, ca)
	if _, err := c.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	ca.SetRateLimited(true)
	_, err := c.NewOrder(context.Background(), []string{"a.test"})
	var p *acme.Problem
	if !errors.As(err, &p) {
		t.Fatalf("want *Problem, got %T %v", err, err)
	}
	if !p.RateLimited() || p.Status != 429 || p.RetryAfter != time.Hour {
		t.Fatalf("%+v", p)
	}
}

func TestACertificateIsObtainedThroughARealHTTP01Door(t *testing.T) {
	ca := acmetest.New(t)
	solver := acme.NewHTTP01()
	ca.ValidateAddr = door(t, solver)
	c := newClient(t, ca)
	if _, err := c.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	certKey, _ := acme.GenerateKey()
	chain, err := c.Obtain(context.Background(), []string{"a.test", "www.a.test"}, certKey, solver)
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(chain)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("no leaf in chain")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "www.a.test", Roots: ca.Pool()}); err != nil {
		t.Fatalf("leaf does not verify: %v", err)
	}
	if next, _ := pem.Decode(rest); next == nil {
		t.Fatal("chain carries no issuer")
	}
	if solver.Pending() != 0 {
		t.Fatal("tokens left on the solver after the order")
	}
	if _, err := tls.X509KeyPair(chain, must(acme.MarshalKeyPEM(certKey))); err != nil {
		t.Fatalf("chain and key do not pair: %v", err)
	}
}

func TestAWrongAnswerAtTheDoorFailsTheAuthorizationWithTheCAsReason(t *testing.T) {
	ca := acmetest.New(t)
	// A door that answers every token with the wrong content.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("nope")) })}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	ca.ValidateAddr = ln.Addr().String()
	c := newClient(t, ca)
	c.Register(context.Background())
	certKey, _ := acme.GenerateKey()
	_, err := c.Obtain(context.Background(), []string{"a.test"}, certKey, acme.NewHTTP01())
	var p *acme.Problem
	if !errors.As(err, &p) || !p.IsCode("unauthorized") {
		t.Fatalf("want the CA's unauthorized problem, got %v", err)
	}
}

func TestTheChallengeCertificateCarriesTheIdentifierExtension(t *testing.T) {
	cert, err := acme.ChallengeCertificate("a.test", "token.thumb")
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	found := false
	for _, e := range leaf.Extensions {
		if e.Id.String() == "1.3.6.1.5.5.7.1.31" && e.Critical {
			found = true
		}
	}
	if !found || len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "a.test" {
		t.Fatalf("extension %v names %v", found, leaf.DNSNames)
	}
	s := acme.NewTLSALPN01()
	s.Present(context.Background(), "A.test", "tok", "token.thumb")
	hello := &tls.ClientHelloInfo{ServerName: "a.test", SupportedProtos: []string{acme.ALPNProto}}
	if s.GetCertificate(hello) == nil {
		t.Fatal("solver does not answer the CA's probe")
	}
	if s.GetCertificate(&tls.ClientHelloInfo{ServerName: "a.test", SupportedProtos: []string{"h2"}}) != nil {
		t.Fatal("a browser must never get the challenge certificate")
	}
}

func TestTheHTTP01SolverNeverPassesAnUnknownTokenThrough(t *testing.T) {
	solver := acme.NewHTTP01()
	addr := door(t, solver)
	solver.Present(context.Background(), "a.test", "good", "good.thumb")
	for path, want := range map[string]int{
		acme.ChallengePath + "good":    200,
		acme.ChallengePath + "unknown": 404,
		acme.ChallengePath:             404,
	} {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s: %d, want %d", path, resp.StatusCode, want)
		}
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
