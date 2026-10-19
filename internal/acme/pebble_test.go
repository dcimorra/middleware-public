package acme_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/acme"
)

// TestPebbleIssuesACertificateThroughHTTP01 runs the client against Pebble,
// Let's Encrypt's test CA, which validates HTTP-01 by really connecting. It
// is skipped unless PEBBLE=1. To run it:
//
//	docker run -d --net=host -e PEBBLE_VA_NOSLEEP=1 ghcr.io/letsencrypt/pebble
//	PEBBLE=1 go test ./internal/acme -run Pebble -v
//
// Pebble listens on https://localhost:14000/dir with a certificate nobody
// trusts (hence the insecure transport, test only) and knocks on port 5002
// for HTTP-01, so the test binds that port. PEBBLE_DOMAIN overrides the
// name ordered (default "localhost"), PEBBLE_DIR the directory URL.
func TestPebbleIssuesACertificateThroughHTTP01(t *testing.T) {
	if os.Getenv("PEBBLE") != "1" {
		t.Skip("set PEBBLE=1 with a Pebble on --net=host to run")
	}
	dir := os.Getenv("PEBBLE_DIR")
	if dir == "" {
		dir = "https://localhost:14000/dir"
	}
	domain := os.Getenv("PEBBLE_DOMAIN")
	if domain == "" {
		domain = "localhost"
	}
	solver := acme.NewHTTP01()
	ln, err := net.Listen("tcp", ":5002")
	if err != nil {
		t.Fatalf("port 5002 is where Pebble validates: %v", err)
	}
	srv := &http.Server{Handler: solver}
	go srv.Serve(ln)
	defer srv.Close()

	key, _ := acme.GenerateKey()
	c := &acme.Client{
		Directory: dir,
		Key:       key,
		HTTPClient: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // Pebble's own self-signed endpoint
		}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := c.Register(ctx, "mailto:test@example.test"); err != nil {
		t.Fatal(err)
	}
	certKey, _ := acme.GenerateKey()
	chain, err := c.Obtain(ctx, []string{domain}, certKey, solver)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(chain)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname(domain); err != nil {
		t.Fatal(err)
	}
	t.Logf("issued %s, expires %s, issuer %s", leaf.DNSNames, leaf.NotAfter, leaf.Issuer.CommonName)
}
