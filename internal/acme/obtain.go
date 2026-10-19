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
	byType := map[string]Solver{}
	for _, s := range solvers {
		byType[s.Type()] = s
	}
	order, err := c.NewOrder(ctx, domains)
	if err != nil {
		return nil, err
	}
	for _, url := range order.Authorizations {
		if err := c.authorize(ctx, url, byType); err != nil {
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
func (c *Client) authorize(ctx context.Context, url string, solvers map[string]Solver) error {
	a, err := c.GetAuthorization(ctx, url)
	if err != nil {
		return err
	}
	if a.Status == "valid" {
		return nil
	}
	domain := a.Identifier.Value
	// Solvers are tried in the order the CA lists the challenges, which
	// for Let's Encrypt puts http-01 first. A challenge the CA already
	// marked invalid is not retried: the whole authorization is dead and
	// a new order is the only way forward.
	for _, ch := range a.Challenges {
		s, ok := solvers[ch.Type]
		if !ok || ch.Status == "invalid" {
			continue
		}
		keyAuth, err := KeyAuthorization(ch.Token, &c.Key.PublicKey)
		if err != nil {
			return err
		}
		if err := s.Present(ctx, domain, ch.Token, keyAuth); err != nil {
			return fmt.Errorf("acme: present %s for %s: %w", ch.Type, domain, err)
		}
		err = c.Accept(ctx, ch)
		if err == nil {
			_, err = c.WaitAuthorization(ctx, url)
		}
		_ = s.CleanUp(context.WithoutCancel(ctx), domain, ch.Token)
		return err
	}
	types := make([]string, 0, len(a.Challenges))
	for _, ch := range a.Challenges {
		types = append(types, ch.Type)
	}
	return fmt.Errorf("acme: no solver for %s; CA offers %v", domain, types)
}
