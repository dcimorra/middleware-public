package acme

import (
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
)

// CreateCSR builds the DER request the CA signs. The CA ignores everything
// but the public key and the names, so the subject is just the first name.
func CreateCSR(key crypto.Signer, domains []string) ([]byte, error) {
	if len(domains) == 0 {
		return nil, errors.New("acme: CSR needs at least one domain")
	}
	tpl := &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: domains[0]},
		DNSNames: domains,
	}
	return x509.CreateCertificateRequest(rand.Reader, tpl, key)
}
