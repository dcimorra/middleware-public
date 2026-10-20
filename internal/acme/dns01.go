package acme

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// DNS01Value is what the TXT record of a dns-01 challenge holds: the
// base64url SHA-256 of the key authorization (RFC 8555 §8.4).
func DNS01Value(keyAuth string) string {
	sum := sha256.Sum256([]byte(keyAuth))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Handler is a solver that only some domains can use. A solver without it
// is taken to handle every domain.
type Handler interface {
	Handles(domain string) bool
}

// DNS01 proves control by writing a TXT record. It is what lets a name get
// its certificate before its DNS points at this machine: a new site on the
// ports door, or a domain still on the tunnel. The provider is plugged in
// as functions, so this package knows no DNS provider.
type DNS01 struct {
	// Covers says whether the provider manages domain's zone.
	Covers func(domain string) bool
	// Set writes a TXT record and returns how to remove it.
	Set func(ctx context.Context, fqdn, value string) (remove func(context.Context) error, err error)
	// Wait returns once the record can be seen by the CA. Nil asks the
	// zone's authoritative name servers until they all answer it.
	Wait func(ctx context.Context, fqdn, value string) error

	mu      sync.Mutex
	removes map[string]func(context.Context) error // by token
}

// Type implements Solver.
func (s *DNS01) Type() string { return "dns-01" }

// Handles implements Handler.
func (s *DNS01) Handles(domain string) bool { return s.Covers != nil && s.Covers(domain) }

// Present implements Solver.
func (s *DNS01) Present(ctx context.Context, domain, token, keyAuth string) error {
	if s.Set == nil {
		return errors.New("acme: dns-01 has no provider")
	}
	fqdn := "_acme-challenge." + strings.TrimSuffix(domain, ".")
	value := DNS01Value(keyAuth)
	remove, err := s.Set(ctx, fqdn, value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.removes == nil {
		s.removes = map[string]func(context.Context) error{}
	}
	s.removes[token] = remove
	s.mu.Unlock()
	wait := s.Wait
	if wait == nil {
		wait = WaitAuthoritative
	}
	return wait(ctx, fqdn, value)
}

// CleanUp implements Solver.
func (s *DNS01) CleanUp(ctx context.Context, _, token string) error {
	s.mu.Lock()
	remove := s.removes[token]
	delete(s.removes, token)
	s.mu.Unlock()
	if remove == nil {
		return nil
	}
	return remove(ctx)
}

// WaitAuthoritative polls the zone's authoritative servers, which is where
// the CA looks, until every one of them answers value for fqdn. Asking a
// recursive resolver instead would cache the "no record" of the first try
// for the zone's negative TTL and make every later poll lie.
func WaitAuthoritative(ctx context.Context, fqdn, value string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	servers, err := authoritative(ctx, fqdn)
	if err != nil {
		return err
	}
	for {
		all := true
		for _, ns := range servers {
			if !hasTXT(ctx, ns, fqdn, value) {
				all = false
				break
			}
		}
		if all {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("acme: %s did not show the challenge on %v in time", fqdn, servers)
		case <-time.After(2 * time.Second):
		}
	}
}

// authoritative finds the name servers of the closest enclosing zone.
func authoritative(ctx context.Context, fqdn string) ([]string, error) {
	labels := strings.Split(strings.TrimSuffix(fqdn, "."), ".")
	for i := 1; i < len(labels)-1; i++ {
		zone := strings.Join(labels[i:], ".")
		nss, err := net.DefaultResolver.LookupNS(ctx, zone)
		if err == nil && len(nss) > 0 {
			out := make([]string, 0, len(nss))
			for _, ns := range nss {
				out = append(out, strings.TrimSuffix(ns.Host, "."))
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("acme: no name servers found for %s", fqdn)
}

func hasTXT(ctx context.Context, server, fqdn, value string) bool {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: 3 * time.Second}
		return d.DialContext(ctx, network, net.JoinHostPort(server, "53"))
	}}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	txts, err := r.LookupTXT(qctx, fqdn)
	if err != nil {
		return false
	}
	for _, t := range txts {
		if t == value {
			return true
		}
	}
	return false
}
