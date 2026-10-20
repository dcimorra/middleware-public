package acme

import (
	"context"
	"crypto"
	"errors"
	"fmt"
)

// Obtain runs one order end to end: authorizations, the first challenge a
// solver can present, finalisation with a CSR for key, and download. The
// PEM chain comes back leaf first. Every authorization is cleaned up even
// when a later one fails, so a token never lingers on a listener.
func (c *Client) Obtain(ctx context.Context, domains []string, key crypto.Signer, solvers ...Solver) ([]byte, error) {
	if len(domains) == 0 {
		return nil, errors.New("acme: nothing to obtain")
	}
	if len(solvers) == 0 {
		return nil, errors.New("acme: no solver")
	}
	order, err := c.NewOrder(ctx, domains)
	if err != nil {
		return nil, err
	}
	for _, url := range order.Authorizations {
		if err := c.authorize(ctx, url, solvers); err != nil {
			return nil, err
		}
	}
	csr, err := CreateCSR(key, domains)
	if err != nil {
		return nil, err
	}
	order, err = c.FinalizeOrder(ctx, order, csr)
	if err != nil {
		return nil, err
	}
	return c.DownloadCertificate(ctx, order.Certificate)
}

// authorize completes one authorization, or returns why it could not.
//
// Solvers are tried in the caller's order, not the CA's: the caller knows
// which proof works from here (dns-01 before the name points at this
// machine; http-01 once it does), and the CA's order (http-01 first, for
// Let's Encrypt) knows nothing about where the name points. A solver that
// does not handle the domain is passed over. A challenge the CA already
// marked invalid is not retried: the whole authorization is dead and a
// new order is the only way forward.
func (c *Client) authorize(ctx context.Context, url string, solvers []Solver) error {
	a, err := c.GetAuthorization(ctx, url)
	if err != nil {
		return err
	}
	if a.Status == "valid" {
		return nil
	}
	domain := a.Identifier.Value
	for _, s := range solvers {
		if h, ok := s.(Handler); ok && !h.Handles(domain) {
			continue
		}
		for _, ch := range a.Challenges {
			if ch.Type != s.Type() || ch.Status == "invalid" {
				continue
			}
			keyAuth, err := KeyAuthorization(ch.Token, &c.Key.PublicKey)
			if err != nil {
				return err
			}
			if err := s.Present(ctx, domain, ch.Token, keyAuth); err != nil {
				_ = s.CleanUp(context.WithoutCancel(ctx), domain, ch.Token)
				return fmt.Errorf("acme: present %s for %s: %w", ch.Type, domain, err)
			}
			err = c.Accept(ctx, ch)
			if err == nil {
				_, err = c.WaitAuthorization(ctx, url)
			}
			_ = s.CleanUp(context.WithoutCancel(ctx), domain, ch.Token)
			return err
		}
	}
	types := make([]string, 0, len(a.Challenges))
	for _, ch := range a.Challenges {
		types = append(types, ch.Type)
	}
	return fmt.Errorf("acme: no solver for %s; CA offers %v", domain, types)
}
