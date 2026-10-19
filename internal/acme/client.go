package acme

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Directory URLs. Production is the default; staging issues certificates
// no browser trusts but has generous limits, which is what you want while
// setting a server up.
const (
	LetsEncrypt        = "https://acme-v02.api.letsencrypt.org/directory"
	LetsEncryptStaging = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

// Directory is the CA's entry point.
type Directory struct {
	NewNonce   string `json:"newNonce"`
	NewAccount string `json:"newAccount"`
	NewOrder   string `json:"newOrder"`
	RevokeCert string `json:"revokeCert"`
	KeyChange  string `json:"keyChange"`
	Meta       struct {
		TermsOfService string `json:"termsOfService"`
	} `json:"meta"`
}

// Client talks to one CA with one account key. It is safe for concurrent
// use; the nonce pool is shared so parallel orders do not each pay a round
// trip for a nonce.
type Client struct {
	// Directory is the directory URL. Empty means Let's Encrypt production.
	Directory string
	// Key is the account key. Register sets KID once the CA knows it.
	Key *ecdsa.PrivateKey
	// KID is the account URL; empty until Register.
	KID string
	// HTTPClient defaults to one with a sensible timeout.
	HTTPClient *http.Client
	// UserAgent is sent with every request; CAs ask for one.
	UserAgent string

	mu     sync.Mutex
	dir    *Directory
	nonces []string
}

// maxBody bounds what is read from the CA: a certificate chain is a few
// kilobytes, and an unbounded read against a misbehaving endpoint is a way
// to run a server out of memory.
const maxBody = 1 << 20

func (c *Client) http() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) directoryURL() string {
	if c.Directory == "" {
		return LetsEncrypt
	}
	return c.Directory
}

// Discover fetches the directory once and caches it.
func (c *Client) Discover(ctx context.Context) (*Directory, error) {
	c.mu.Lock()
	if c.dir != nil {
		d := c.dir
		c.mu.Unlock()
		return d, nil
	}
	c.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.directoryURL(), nil)
	if err != nil {
		return nil, err
	}
	resp, body, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("acme: directory: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseProblem(resp, body)
	}
	var d Directory
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("acme: directory: %w", err)
	}
	if d.NewNonce == "" || d.NewAccount == "" || d.NewOrder == "" {
		return nil, errors.New("acme: directory is missing newNonce, newAccount or newOrder")
	}
	c.mu.Lock()
	c.dir = &d
	c.mu.Unlock()
	return &d, nil
}

// do sends a request, reads the body and keeps any nonce the CA attached.
// Every ACME response carries Replay-Nonce, so the pool refills itself.
func (c *Client) do(req *http.Request) (*http.Response, []byte, error) {
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, nil, err
	}
	if n := resp.Header.Get("Replay-Nonce"); n != "" {
		c.mu.Lock()
		c.nonces = append(c.nonces, n)
		c.mu.Unlock()
	}
	return resp, body, nil
}

func (c *Client) nonce(ctx context.Context) (string, error) {
	c.mu.Lock()
	if n := len(c.nonces); n > 0 {
		v := c.nonces[n-1]
		c.nonces = c.nonces[:n-1]
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()
	d, err := c.Discover(ctx)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, d.NewNonce, nil)
	if err != nil {
		return "", err
	}
	resp, _, err := c.do(req)
	if err != nil {
		return "", fmt.Errorf("acme: nonce: %w", err)
	}
	n := resp.Header.Get("Replay-Nonce")
	if n == "" {
		return "", errors.New("acme: newNonce answered without Replay-Nonce")
	}
	// do already pooled it; take it back out so it is used exactly once.
	c.mu.Lock()
	if l := len(c.nonces); l > 0 && c.nonces[l-1] == n {
		c.nonces = c.nonces[:l-1]
	}
	c.mu.Unlock()
	return n, nil
}

// post signs payload and sends it. A nil payload is POST-as-GET. The
// response is decoded into out when it is non-nil. A badNonce answer is
// retried with the nonce that very answer carries: the RFC requires the CA
// to attach a fresh one, and a client that gives up there fails every time
// the pool went stale across a long wait.
func (c *Client) post(ctx context.Context, url string, payload any, out any) (*http.Response, error) {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}
	var last error
	for attempt := 0; attempt < 4; attempt++ {
		n, err := c.nonce(ctx)
		if err != nil {
			return nil, err
		}
		kid := c.KID
		if url == c.accountURL() {
			kid = ""
		}
		jws, err := signJWS(c.Key, url, n, kid, body)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jws))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/jose+json")
		resp, data, err := c.do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			p := parseProblem(resp, data)
			if p.IsCode("badNonce") {
				last = p
				continue
			}
			return resp, p
		}
		if out != nil && len(data) > 0 {
			if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/json") || ct == "" {
				if err := json.Unmarshal(data, out); err != nil {
					return resp, fmt.Errorf("acme: %s: %w", url, err)
				}
			} else if raw, ok := out.(*[]byte); ok {
				*raw = data
			}
		}
		return resp, nil
	}
	return nil, fmt.Errorf("acme: giving up after repeated badNonce: %w", last)
}

// accountURL is the only URL signed with the embedded key instead of the
// account id: the account does not exist yet when it is called.
func (c *Client) accountURL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dir == nil {
		return ""
	}
	return c.dir.NewAccount
}

// Account is what the CA knows about us.
type Account struct {
	URL     string   `json:"-"`
	Status  string   `json:"status"`
	Contact []string `json:"contact,omitempty"`
	Orders  string   `json:"orders,omitempty"`
}

// Register creates the account for Key, agreeing to the terms of service,
// or finds the existing one for the same key: the CA answers 200 instead
// of 201 and both carry the account URL in Location. Terms are agreed
// without showing them because there is nobody to show them to on a
// server; the directory's termsOfService URL is in Discover for the panel.
func (c *Client) Register(ctx context.Context, contact ...string) (*Account, error) {
	if c.Key == nil {
		return nil, errors.New("acme: no account key")
	}
	d, err := c.Discover(ctx)
	if err != nil {
		return nil, err
	}
	req := map[string]any{"termsOfServiceAgreed": true}
	if len(contact) > 0 {
		req["contact"] = contact
	}
	var acct Account
	resp, err := c.post(ctx, d.NewAccount, req, &acct)
	if err != nil {
		return nil, err
	}
	acct.URL = resp.Header.Get("Location")
	if acct.URL == "" {
		return nil, errors.New("acme: newAccount answered without Location")
	}
	c.KID = acct.URL
	return &acct, nil
}

// Identifier names what a certificate is for.
type Identifier struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Order is one certificate request from creation to download.
type Order struct {
	URL            string       `json:"-"`
	Status         string       `json:"status"`
	Expires        string       `json:"expires,omitempty"`
	Identifiers    []Identifier `json:"identifiers"`
	Authorizations []string     `json:"authorizations"`
	Finalize       string       `json:"finalize"`
	Certificate    string       `json:"certificate,omitempty"`
	Error          *Problem     `json:"error,omitempty"`
}

// Authorization is the CA's challenge set for one identifier.
type Authorization struct {
	URL        string      `json:"-"`
	Identifier Identifier  `json:"identifier"`
	Status     string      `json:"status"`
	Expires    string      `json:"expires,omitempty"`
	Challenges []Challenge `json:"challenges"`
	Wildcard   bool        `json:"wildcard,omitempty"`
}

// Challenge is one way to prove control.
type Challenge struct {
	Type   string   `json:"type"`
	URL    string   `json:"url"`
	Token  string   `json:"token"`
	Status string   `json:"status"`
	Error  *Problem `json:"error,omitempty"`
}

// NewOrder asks for a certificate covering domains.
func (c *Client) NewOrder(ctx context.Context, domains []string) (*Order, error) {
	if c.KID == "" {
		return nil, errors.New("acme: Register before NewOrder")
	}
	d, err := c.Discover(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]Identifier, 0, len(domains))
	for _, dom := range domains {
		ids = append(ids, Identifier{Type: "dns", Value: dom})
	}
	var o Order
	resp, err := c.post(ctx, d.NewOrder, map[string]any{"identifiers": ids}, &o)
	if err != nil {
		return nil, err
	}
	o.URL = resp.Header.Get("Location")
	if o.URL == "" {
		return nil, errors.New("acme: newOrder answered without Location")
	}
	return &o, nil
}

// GetOrder refreshes an order.
func (c *Client) GetOrder(ctx context.Context, url string) (*Order, error) {
	var o Order
	if _, err := c.post(ctx, url, nil, &o); err != nil {
		return nil, err
	}
	o.URL = url
	return &o, nil
}

// GetAuthorization fetches an authorization.
func (c *Client) GetAuthorization(ctx context.Context, url string) (*Authorization, error) {
	var a Authorization
	if _, err := c.post(ctx, url, nil, &a); err != nil {
		return nil, err
	}
	a.URL = url
	return &a, nil
}

// Accept tells the CA the challenge is ready to be checked. The body is an
// empty object, not empty: that is what distinguishes it from POST-as-GET.
func (c *Client) Accept(ctx context.Context, ch Challenge) error {
	_, err := c.post(ctx, ch.URL, map[string]any{}, nil)
	return err
}

// pollInterval is how fast to ask again when the CA sent no Retry-After.
var pollInterval = 2 * time.Second

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// WaitAuthorization polls until the authorization leaves "pending" and
// "processing". An invalid one returns the challenge's problem, which is
// where the CA says what it could not reach.
func (c *Client) WaitAuthorization(ctx context.Context, url string) (*Authorization, error) {
	for {
		var a Authorization
		resp, err := c.post(ctx, url, nil, &a)
		if err != nil {
			return nil, err
		}
		a.URL = url
		switch a.Status {
		case "valid":
			return &a, nil
		case "pending", "processing":
		default:
			for _, ch := range a.Challenges {
				if ch.Error != nil {
					return &a, ch.Error
				}
			}
			return &a, fmt.Errorf("acme: authorization for %s is %s", a.Identifier.Value, a.Status)
		}
		wait := retryAfter(resp.Header)
		if wait == 0 {
			wait = pollInterval
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// FinalizeOrder submits the CSR and polls until the order is valid.
func (c *Client) FinalizeOrder(ctx context.Context, o *Order, csrDER []byte) (*Order, error) {
	var out Order
	if _, err := c.post(ctx, o.Finalize, map[string]string{"csr": b64(csrDER)}, &out); err != nil {
		return nil, err
	}
	out.URL = o.URL
	return c.WaitOrder(ctx, o.URL)
}

// WaitOrder polls until the order is valid or fails.
func (c *Client) WaitOrder(ctx context.Context, url string) (*Order, error) {
	for {
		var o Order
		resp, err := c.post(ctx, url, nil, &o)
		if err != nil {
			return nil, err
		}
		o.URL = url
		switch o.Status {
		case "valid":
			if o.Certificate == "" {
				return &o, errors.New("acme: order is valid but names no certificate")
			}
			return &o, nil
		case "pending", "ready", "processing":
		default:
			if o.Error != nil {
				return &o, o.Error
			}
			return &o, fmt.Errorf("acme: order is %s", o.Status)
		}
		wait := retryAfter(resp.Header)
		if wait == 0 {
			wait = pollInterval
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// DownloadCertificate returns the PEM chain, leaf first.
func (c *Client) DownloadCertificate(ctx context.Context, url string) ([]byte, error) {
	var raw []byte
	if _, err := c.post(ctx, url, nil, &raw); err != nil {
		return nil, err
	}
	if !bytes.Contains(raw, []byte("-----BEGIN CERTIFICATE-----")) {
		return nil, errors.New("acme: certificate download is not a PEM chain")
	}
	return raw, nil
}
