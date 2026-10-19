package certs

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/dcimorra/middleware-public/internal/acme"
	"github.com/dcimorra/middleware-public/internal/fsx"
)

// ACMEIssuer obtains certificates from a CA with one account kept under
// Dir: account.key (the P-256 key, 0600) and account.json (the directory
// it was registered with and the account URL). Changing the directory,
// say from staging to production, registers again with the same key.
type ACMEIssuer struct {
	// Dir is where the account lives, normally data/certs.
	Dir string
	// Directory is the CA's directory URL; empty is Let's Encrypt.
	Directory string
	// Email is the account contact. Optional but worth setting: it is
	// where the CA writes before a certificate expires unrenewed.
	Email string
	// HTTPClient overrides the client's transport (tests, proxies).
	HTTPClient *http.Client
	// HTTP01 answers on every door; required.
	HTTP01 *acme.HTTP01
	// TLSALPN answers on 443; optional.
	TLSALPN *acme.TLSALPN01
	// UserAgent is what the CA logs.
	UserAgent string

	mu     sync.Mutex
	client *acme.Client
}

type accountFile struct {
	Directory string `json:"directory"`
	KID       string `json:"kid"`
}

// Issue implements Issuer.
func (i *ACMEIssuer) Issue(ctx context.Context, domain string, key crypto.Signer) ([]byte, error) {
	c, err := i.account(ctx)
	if err != nil {
		return nil, err
	}
	var solvers []acme.Solver
	if i.HTTP01 != nil {
		solvers = append(solvers, i.HTTP01)
	}
	if i.TLSALPN != nil {
		solvers = append(solvers, i.TLSALPN)
	}
	if len(solvers) == 0 {
		return nil, errors.New("certs: issuer has no solver")
	}
	return c.Obtain(ctx, []string{domain}, key, solvers...)
}

func (i *ACMEIssuer) directory() string {
	if i.Directory == "" {
		return acme.LetsEncrypt
	}
	return i.Directory
}

// account returns the registered client, registering on first use. The
// lock covers registration so two first requests do not create two
// accounts; the CA would accept both and we would keep one.
func (i *ACMEIssuer) account(ctx context.Context) (*acme.Client, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.client != nil {
		return i.client, nil
	}
	if i.Dir == "" {
		return nil, errors.New("certs: issuer has no directory to keep its account in")
	}
	if err := os.MkdirAll(i.Dir, 0o700); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(i.Dir, "account.key")
	data, err := os.ReadFile(keyPath)
	c := &acme.Client{Directory: i.directory(), HTTPClient: i.HTTPClient, UserAgent: i.UserAgent}
	switch {
	case err == nil:
		c.Key, err = acme.ParseKeyPEM(data)
		if err != nil {
			return nil, fmt.Errorf("certs: %s: %w", keyPath, err)
		}
	case os.IsNotExist(err):
		c.Key, err = acme.GenerateKey()
		if err != nil {
			return nil, err
		}
		pemData, err := acme.MarshalKeyPEM(c.Key)
		if err != nil {
			return nil, err
		}
		if err := fsx.WriteAtomic(keyPath, pemData, 0o600); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	var acct accountFile
	acctPath := filepath.Join(i.Dir, "account.json")
	if err := fsx.ReadJSON(acctPath, &acct); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if acct.KID != "" && acct.Directory == c.Directory {
		c.KID = acct.KID
	} else {
		var contact []string
		if i.Email != "" {
			contact = []string{"mailto:" + i.Email}
		}
		if _, err := c.Register(ctx, contact...); err != nil {
			return nil, err
		}
		if err := fsx.WriteJSON(acctPath, accountFile{Directory: c.Directory, KID: c.KID}, 0o600); err != nil {
			return nil, err
		}
	}
	i.client = c
	return c, nil
}
