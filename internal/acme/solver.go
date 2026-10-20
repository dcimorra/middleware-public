package acme

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ChallengePath is where HTTP-01 is served. It is answered on every door
// before anything else looks at the request, because a domain moving from
// the tunnel to the ports gets its certificate while its DNS still points
// at the tunnel.
const ChallengePath = "/.well-known/acme-challenge/"

// Solver presents a challenge for the CA to find.
type Solver interface {
	// Type is "http-01", "tls-alpn-01" or "dns-01".
	Type() string
	Present(ctx context.Context, domain, token, keyAuth string) error
	CleanUp(ctx context.Context, domain, token string) error
}

// HTTP01 serves key authorisations under ChallengePath. One instance is
// shared by every listener in the process.
type HTTP01 struct {
	mu     sync.RWMutex
	tokens map[string]string // token → key authorisation
}

// NewHTTP01 returns an empty solver.
func NewHTTP01() *HTTP01 { return &HTTP01{tokens: map[string]string{}} }

// Type implements Solver.
func (s *HTTP01) Type() string { return "http-01" }

// Present implements Solver.
func (s *HTTP01) Present(_ context.Context, _, token, keyAuth string) error {
	s.mu.Lock()
	s.tokens[token] = keyAuth
	s.mu.Unlock()
	return nil
}

// CleanUp implements Solver.
func (s *HTTP01) CleanUp(_ context.Context, _, token string) error {
	s.mu.Lock()
	delete(s.tokens, token)
	s.mu.Unlock()
	return nil
}

// Pending reports whether any challenge is being served, so a listener
// can tell "nothing to answer" from "answered wrong".
func (s *HTTP01) Pending() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tokens)
}

// ServeHTTP answers the CA. An unknown token is a 404 rather than a pass
// through to the site: a site must never be able to answer an ACME
// challenge on its own, or a compromised site could obtain certificates
// for its own name through this server's account.
func (s *HTTP01) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, ChallengePath)
	if token == "" || strings.ContainsAny(token, "/\\") {
		http.NotFound(w, r)
		return
	}
	s.mu.RLock()
	keyAuth, ok := s.tokens[token]
	s.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(keyAuth))
}

// ALPNProto is the protocol a CA negotiates for TLS-ALPN-01.
const ALPNProto = "acme-tls/1"

// idPeACMEIdentifier is the extension (RFC 8737 §6.1) that carries the
// hash of the key authorisation inside the challenge certificate.
var idPeACMEIdentifier = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 31}

// TLSALPN01 keeps the self-signed challenge certificates a 443 listener
// hands out when a client asks for ALPNProto. It is the fallback for a
// server whose port 80 is blocked by a provider.
type TLSALPN01 struct {
	mu    sync.RWMutex
	certs map[string]*tls.Certificate // domain → challenge certificate
}

// NewTLSALPN01 returns an empty solver.
func NewTLSALPN01() *TLSALPN01 { return &TLSALPN01{certs: map[string]*tls.Certificate{}} }

// Type implements Solver.
func (s *TLSALPN01) Type() string { return "tls-alpn-01" }

// Present implements Solver.
func (s *TLSALPN01) Present(_ context.Context, domain, _, keyAuth string) error {
	cert, err := ChallengeCertificate(domain, keyAuth)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.certs[strings.ToLower(domain)] = cert
	s.mu.Unlock()
	return nil
}

// CleanUp implements Solver.
func (s *TLSALPN01) CleanUp(_ context.Context, domain, _ string) error {
	s.mu.Lock()
	delete(s.certs, strings.ToLower(domain))
	s.mu.Unlock()
	return nil
}

// Wants reports whether the handshake is a CA probing this solver.
func Wants(hello *tls.ClientHelloInfo) bool {
	for _, p := range hello.SupportedProtos {
		if p == ALPNProto {
			return true
		}
	}
	return false
}

// GetCertificate answers a CA's probe; nil when no challenge is pending
// for that name, so the caller falls back to its normal certificate.
func (s *TLSALPN01) GetCertificate(hello *tls.ClientHelloInfo) *tls.Certificate {
	if !Wants(hello) {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.certs[strings.ToLower(hello.ServerName)]
}

// ChallengeCertificate builds the certificate RFC 8737 describes: one
// SAN, the acmeIdentifier extension marked critical with the SHA-256 of
// the key authorisation, self-signed with a throwaway key.
func ChallengeCertificate(domain, keyAuth string) (*tls.Certificate, error) {
	key, err := GenerateKey()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(keyAuth))
	ext, err := asn1.Marshal(sum[:])
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:    serial,
		Subject:         pkix.Name{CommonName: domain},
		DNSNames:        []string{domain},
		NotBefore:       time.Now().Add(-time.Hour),
		NotAfter:        time.Now().Add(24 * time.Hour),
		KeyUsage:        x509.KeyUsageDigitalSignature,
		ExtKeyUsage:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		ExtraExtensions: []pkix.Extension{{Id: idPeACMEIdentifier, Critical: true, Value: ext}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

var _ interface {
	GetCertificate(*tls.ClientHelloInfo) *tls.Certificate
} = (*TLSALPN01)(nil)

var _ Solver = (*HTTP01)(nil)
var _ Solver = (*TLSALPN01)(nil)
