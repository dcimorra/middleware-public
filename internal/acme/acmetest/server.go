// Package acmetest is a fake CA for tests: enough of RFC 8555 to drive the
// client through a whole order, with real nonces that may be used once,
// real JWS verification and real certificates signed by a throwaway root.
// Pebble does the same more faithfully but needs Docker; this runs inside
// `go test` and lets the certs and inbound packages prove their listeners
// answer a validation that actually knocks on the door.
package acmetest

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/acme"
)

// Server is the fake CA. Fields are read at request time, so a test may
// change them between calls.
type Server struct {
	// URL is the directory URL.
	URL string
	// Root signs every certificate; Pool returns it for verification.
	Root *x509.Certificate
	// Validity of issued certificates. Default 90 days.
	Validity time.Duration
	// ValidateAddr is the host:port the CA knocks on for HTTP-01, with
	// the domain as Host header. Empty means every challenge passes
	// without a request, for tests that are not about the door.
	ValidateAddr string
	// TXT, when set, makes the CA offer dns-01 too (after http-01, as
	// Let's Encrypt lists them) and validate it by looking up the TXT
	// records of _acme-challenge.<domain> through this function.
	TXT func(fqdn string) []string

	rootKey *ecdsa.PrivateKey
	srv     *httptest.Server

	mu          sync.Mutex
	next        int
	nonces      map[string]bool
	accounts    map[string]*ecdsa.PublicKey // kid → key
	orders      map[int]*order
	authzs      map[int]*authz
	rejectNonce int
	rateLimited bool
	issued      int
	dnsSolved   int
}

type order struct {
	id      int
	kid     string
	domains []string
	status  string
	authzs  []int
	certPEM []byte
}

type authz struct {
	id      int
	order   int
	domain  string
	status  string
	token   string
	problem *acme.Problem
}

// New starts the CA and stops it when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	key, err := acme.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "acmetest root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour * 365),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Root:     root,
		Validity: 90 * 24 * time.Hour,
		rootKey:  key,
		nonces:   map[string]bool{},
		accounts: map[string]*ecdsa.PublicKey{},
		orders:   map[int]*order{},
		authzs:   map[int]*authz{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dir", s.directory)
	mux.HandleFunc("HEAD /nonce", s.newNonce)
	mux.HandleFunc("GET /nonce", s.newNonce)
	mux.HandleFunc("POST /new-account", s.newAccount)
	mux.HandleFunc("POST /new-order", s.newOrder)
	mux.HandleFunc("POST /order/{id}", s.getOrder)
	mux.HandleFunc("POST /authz/{id}", s.getAuthz)
	mux.HandleFunc("POST /chall/{id}/{type}", s.challenge)
	mux.HandleFunc("POST /finalize/{id}", s.finalize)
	mux.HandleFunc("POST /cert/{id}", s.certificate)
	s.srv = httptest.NewServer(mux)
	s.URL = s.srv.URL + "/dir"
	t.Cleanup(s.srv.Close)
	return s
}

// Pool holds the root, for a tls.Config that verifies what was issued.
func (s *Server) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(s.Root)
	return p
}

// RejectNextNonce makes the next signed request fail with badNonce, the
// way a real CA does when a pooled nonce went stale.
func (s *Server) RejectNextNonce(n int) {
	s.mu.Lock()
	s.rejectNonce = n
	s.mu.Unlock()
}

// SetRateLimited makes every new order answer 429 rateLimited.
func (s *Server) SetRateLimited(on bool) {
	s.mu.Lock()
	s.rateLimited = on
	s.mu.Unlock()
}

// Issued counts certificates signed so far.
func (s *Server) Issued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issued
}

func (s *Server) url(path string) string { return s.srv.URL + path }

func (s *Server) nonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	n := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	s.nonces[n] = true
	s.mu.Unlock()
	return n
}

func (s *Server) directory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"newNonce":   s.url("/nonce"),
		"newAccount": s.url("/new-account"),
		"newOrder":   s.url("/new-order"),
		"meta":       map[string]string{"termsOfService": s.url("/terms")},
	})
}

func (s *Server) newNonce(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Replay-Nonce", s.nonce())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) problem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Replay-Nonce", s.nonce())
	if code == "rateLimited" {
		w.Header().Set("Retry-After", "3600")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "urn:ietf:params:acme:error:" + code, "detail": detail, "status": status,
	})
}

type jwsEnvelope struct {
	Protected string `json:"protected"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

type protectedHeader struct {
	Alg   string            `json:"alg"`
	Nonce string            `json:"nonce"`
	URL   string            `json:"url"`
	KID   string            `json:"kid"`
	JWK   map[string]string `json:"jwk"`
}

// verify checks the envelope the way a CA would, and answers the problem
// itself when something is wrong. It returns the decoded payload, the
// account id (the kid, or the jwk's thumbprint for a new account) and the
// public key.
func (s *Server) verify(w http.ResponseWriter, r *http.Request) ([]byte, string, *ecdsa.PublicKey, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		s.problem(w, 400, "malformed", err.Error())
		return nil, "", nil, false
	}
	var env jwsEnvelope
	if err := json.Unmarshal(body, &env); err != nil || r.Header.Get("Content-Type") != "application/jose+json" {
		s.problem(w, 400, "malformed", "not a JWS")
		return nil, "", nil, false
	}
	ph, err := base64.RawURLEncoding.DecodeString(env.Protected)
	if err != nil {
		s.problem(w, 400, "malformed", "protected header")
		return nil, "", nil, false
	}
	var h protectedHeader
	if err := json.Unmarshal(ph, &h); err != nil || h.Alg != "ES256" {
		s.problem(w, 400, "badSignatureAlgorithm", "want ES256")
		return nil, "", nil, false
	}
	if h.URL != s.srv.URL+r.URL.Path {
		s.problem(w, 400, "unauthorized", "url mismatch: "+h.URL)
		return nil, "", nil, false
	}
	s.mu.Lock()
	reject := s.rejectNonce > 0
	if reject {
		s.rejectNonce--
	}
	fresh := s.nonces[h.Nonce]
	delete(s.nonces, h.Nonce)
	s.mu.Unlock()
	if reject || !fresh {
		s.problem(w, 400, "badNonce", "nonce "+h.Nonce+" is not valid")
		return nil, "", nil, false
	}
	var pub *ecdsa.PublicKey
	var id string
	if h.KID != "" && h.JWK != nil {
		s.problem(w, 400, "malformed", "jwk and kid together")
		return nil, "", nil, false
	}
	if h.KID != "" {
		s.mu.Lock()
		pub = s.accounts[h.KID]
		s.mu.Unlock()
		if pub == nil {
			s.problem(w, 400, "accountDoesNotExist", h.KID)
			return nil, "", nil, false
		}
		id = h.KID
	} else {
		pub, err = acme.ParseJWK(h.JWK)
		if err != nil {
			s.problem(w, 400, "badPublicKey", err.Error())
			return nil, "", nil, false
		}
		tp, _ := acme.Thumbprint(pub)
		id = s.url("/acct/" + tp)
	}
	sig, err := base64.RawURLEncoding.DecodeString(env.Signature)
	if err != nil || !acme.VerifyES256(pub, env.Protected+"."+env.Payload, sig) {
		s.problem(w, 400, "unauthorized", "bad signature")
		return nil, "", nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(env.Payload)
	if err != nil {
		s.problem(w, 400, "malformed", "payload")
		return nil, "", nil, false
	}
	return payload, id, pub, true
}

func (s *Server) reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Replay-Nonce", s.nonce())
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) newAccount(w http.ResponseWriter, r *http.Request) {
	payload, id, pub, ok := s.verify(w, r)
	if !ok {
		return
	}
	var req struct {
		Agreed  bool     `json:"termsOfServiceAgreed"`
		Contact []string `json:"contact"`
	}
	_ = json.Unmarshal(payload, &req)
	if !req.Agreed {
		s.problem(w, 400, "userActionRequired", "terms not agreed")
		return
	}
	s.mu.Lock()
	_, existed := s.accounts[id]
	s.accounts[id] = pub
	s.mu.Unlock()
	w.Header().Set("Location", id)
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	s.reply(w, status, map[string]any{"status": "valid", "contact": req.Contact})
}

func (s *Server) newOrder(w http.ResponseWriter, r *http.Request) {
	payload, kid, _, ok := s.verify(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	limited := s.rateLimited
	s.mu.Unlock()
	if limited {
		s.problem(w, http.StatusTooManyRequests, "rateLimited", "too many certificates already issued")
		return
	}
	var req struct {
		Identifiers []acme.Identifier `json:"identifiers"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || len(req.Identifiers) == 0 {
		s.problem(w, 400, "malformed", "identifiers")
		return
	}
	s.mu.Lock()
	s.next++
	o := &order{id: s.next, kid: kid, status: "pending"}
	for _, id := range req.Identifiers {
		if id.Type != "dns" || !strings.Contains(id.Value, ".") {
			s.mu.Unlock()
			s.problem(w, 400, "rejectedIdentifier", id.Value)
			return
		}
		s.next++
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		a := &authz{id: s.next, order: o.id, domain: id.Value, status: "pending", token: base64.RawURLEncoding.EncodeToString(b)}
		s.authzs[a.id] = a
		o.authzs = append(o.authzs, a.id)
		o.domains = append(o.domains, id.Value)
	}
	s.orders[o.id] = o
	s.mu.Unlock()
	w.Header().Set("Location", s.url("/order/"+strconv.Itoa(o.id)))
	s.reply(w, http.StatusCreated, s.orderJSON(o))
}

func (s *Server) orderJSON(o *order) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := []acme.Identifier{}
	for _, d := range o.domains {
		ids = append(ids, acme.Identifier{Type: "dns", Value: d})
	}
	az := []string{}
	for _, id := range o.authzs {
		az = append(az, s.url("/authz/"+strconv.Itoa(id)))
	}
	out := map[string]any{
		"status": o.status, "identifiers": ids, "authorizations": az,
		"finalize": s.url("/finalize/" + strconv.Itoa(o.id)),
	}
	if o.certPEM != nil {
		out["certificate"] = s.url("/cert/" + strconv.Itoa(o.id))
	}
	return out
}

func (s *Server) lookupOrder(w http.ResponseWriter, r *http.Request, kid string) *order {
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.mu.Lock()
	o := s.orders[id]
	s.mu.Unlock()
	if o == nil || o.kid != kid {
		s.problem(w, 404, "malformed", "no such order")
		return nil
	}
	return o
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	_, kid, _, ok := s.verify(w, r)
	if !ok {
		return
	}
	o := s.lookupOrder(w, r, kid)
	if o == nil {
		return
	}
	s.reply(w, 200, s.orderJSON(o))
}

func (s *Server) authzJSON(a *authz) map[string]any {
	types := []string{"http-01"}
	if s.TXT != nil {
		types = append(types, "dns-01")
	}
	var chs []any
	for _, typ := range types {
		ch := map[string]any{
			"type": typ, "url": s.url("/chall/" + strconv.Itoa(a.id) + "/" + typ), "token": a.token, "status": a.status,
		}
		if a.problem != nil {
			ch["error"] = a.problem
		}
		chs = append(chs, ch)
	}
	return map[string]any{
		"identifier": acme.Identifier{Type: "dns", Value: a.domain},
		"status":     a.status,
		"challenges": chs,
	}
}

func (s *Server) lookupAuthz(w http.ResponseWriter, r *http.Request, kid string) *authz {
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.mu.Lock()
	a := s.authzs[id]
	var o *order
	if a != nil {
		o = s.orders[a.order]
	}
	s.mu.Unlock()
	if a == nil || o == nil || o.kid != kid {
		s.problem(w, 404, "malformed", "no such authorization")
		return nil
	}
	return a
}

func (s *Server) getAuthz(w http.ResponseWriter, r *http.Request) {
	_, kid, _, ok := s.verify(w, r)
	if !ok {
		return
	}
	a := s.lookupAuthz(w, r, kid)
	if a == nil {
		return
	}
	s.mu.Lock()
	out := s.authzJSON(a)
	s.mu.Unlock()
	s.reply(w, 200, out)
}

// challenge validates synchronously: the client is polling anyway, and a
// goroutine here would only add a race to the test.
func (s *Server) challenge(w http.ResponseWriter, r *http.Request) {
	payload, kid, pub, ok := s.verify(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(string(payload)) != "{}" {
		s.problem(w, 400, "malformed", "challenge body must be {}")
		return
	}
	a := s.lookupAuthz(w, r, kid)
	if a == nil {
		return
	}
	want, _ := acme.KeyAuthorization(a.token, pub)
	s.mu.Lock()
	addr := s.ValidateAddr
	txt := s.TXT
	s.mu.Unlock()
	var problem *acme.Problem
	typ := r.PathValue("type")
	if typ == "dns-01" {
		s.mu.Lock()
		s.dnsSolved++
		s.mu.Unlock()
		digest := acme.DNS01Value(want)
		found := false
		for _, v := range txt("_acme-challenge." + a.domain) {
			found = found || v == digest
		}
		if !found {
			problem = &acme.Problem{Type: "urn:ietf:params:acme:error:unauthorized", Detail: "no TXT record with the key authorization digest", Status: 403}
		}
	} else if addr != "" {
		got, err := fetch(addr, a.domain, a.token)
		if err != nil {
			problem = &acme.Problem{Type: "urn:ietf:params:acme:error:connection", Detail: err.Error(), Status: 400}
		} else if got != want {
			problem = &acme.Problem{Type: "urn:ietf:params:acme:error:unauthorized", Detail: fmt.Sprintf("key authorization mismatch: got %q", got), Status: 403}
		}
	}
	s.mu.Lock()
	if problem != nil {
		a.status, a.problem = "invalid", problem
		s.orders[a.order].status = "invalid"
	} else {
		a.status = "valid"
		o := s.orders[a.order]
		ready := true
		for _, id := range o.authzs {
			if s.authzs[id].status != "valid" {
				ready = false
			}
		}
		if ready {
			o.status = "ready"
		}
	}
	out := s.authzJSON(a)
	s.mu.Unlock()
	for _, ch := range out["challenges"].([]any) {
		if ch.(map[string]any)["type"] == typ {
			s.reply(w, 200, ch)
			return
		}
	}
	s.reply(w, 200, out["challenges"].([]any)[0])
}

// DNSSolved counts dns-01 validations, so a test can tell which
// challenge the client chose.
func (s *Server) DNSSolved() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dnsSolved
}

func fetch(addr, domain, token string) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+acme.ChallengePath+token, nil)
	if err != nil {
		return "", err
	}
	req.Host = domain
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	return strings.TrimSpace(string(body)), nil
}

func (s *Server) finalize(w http.ResponseWriter, r *http.Request) {
	payload, kid, _, ok := s.verify(w, r)
	if !ok {
		return
	}
	o := s.lookupOrder(w, r, kid)
	if o == nil {
		return
	}
	var req struct {
		CSR string `json:"csr"`
	}
	_ = json.Unmarshal(payload, &req)
	der, err := base64.RawURLEncoding.DecodeString(req.CSR)
	if err != nil {
		s.problem(w, 400, "badCSR", "not base64url")
		return
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil || csr.CheckSignature() != nil {
		s.problem(w, 400, "badCSR", "cannot parse")
		return
	}
	s.mu.Lock()
	if o.status != "ready" {
		st := o.status
		s.mu.Unlock()
		s.problem(w, 403, "orderNotReady", "order is "+st)
		return
	}
	names := map[string]bool{}
	for _, d := range o.domains {
		names[d] = true
	}
	for _, n := range csr.DNSNames {
		if !names[n] {
			s.mu.Unlock()
			s.problem(w, 400, "badCSR", "name not in order: "+n)
			return
		}
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: o.domains[0]},
		DNSNames:     o.domains,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(s.Validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leaf, err := x509.CreateCertificate(rand.Reader, tpl, s.Root, csr.PublicKey, s.rootKey)
	if err != nil {
		s.mu.Unlock()
		s.problem(w, 500, "serverInternal", err.Error())
		return
	}
	o.certPEM = append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Root.Raw})...)
	o.status = "valid"
	s.issued++
	s.mu.Unlock()
	s.reply(w, 200, s.orderJSON(o))
}

func (s *Server) certificate(w http.ResponseWriter, r *http.Request) {
	_, kid, _, ok := s.verify(w, r)
	if !ok {
		return
	}
	o := s.lookupOrder(w, r, kid)
	if o == nil {
		return
	}
	s.mu.Lock()
	pemChain := o.certPEM
	s.mu.Unlock()
	if pemChain == nil {
		s.problem(w, 403, "orderNotReady", "no certificate yet")
		return
	}
	w.Header().Set("Content-Type", "application/pem-certificate-chain")
	w.Header().Set("Replay-Nonce", s.nonce())
	_, _ = w.Write(pemChain)
}
