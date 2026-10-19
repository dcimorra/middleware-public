// Package acme is an RFC 8555 client on the standard library: account keys,
// JWS signing, orders, HTTP-01 and TLS-ALPN-01 challenges, finalisation and
// download. It exists because the core takes no third-party dependency, and
// the subset a single server needs (one account, one domain per order) is a
// few hundred lines once the protocol is read carefully.
package acme

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
)

// base64url without padding, as every JOSE field in ACME is encoded.
func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwk is the public account key as the CA sees it. Only P-256 is supported:
// Let's Encrypt and Pebble both accept it, and one curve means one code path.
func jwk(pub *ecdsa.PublicKey) map[string]string {
	// Coordinates are padded to the curve size: a leading zero byte dropped
	// by big.Int.Bytes would make the CA reject the key as malformed, and
	// it happens for roughly one key in 256.
	size := (pub.Curve.Params().BitSize + 7) / 8
	return map[string]string{
		"crv": "P-256",
		"kty": "EC",
		"x":   b64(pub.X.FillBytes(make([]byte, size))),
		"y":   b64(pub.Y.FillBytes(make([]byte, size))),
	}
}

// Thumbprint is the RFC 7638 key thumbprint: SHA-256 over the JWK with its
// required members in lexicographic order and no whitespace, which is what
// encoding/json produces for a map.
func Thumbprint(pub *ecdsa.PublicKey) (string, error) {
	data, err := json.Marshal(jwk(pub))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return b64(sum[:]), nil
}

// signJWS builds the flattened JSON serialisation the CA expects. A new
// account proves possession of the key by embedding it (jwk); every later
// request names the account URL instead (kid). Both at once is an error in
// the RFC, so exactly one is written.
func signJWS(key *ecdsa.PrivateKey, url, nonce, kid string, payload []byte) ([]byte, error) {
	if key == nil || key.Curve != elliptic.P256() {
		return nil, errors.New("acme: account key must be ECDSA P-256")
	}
	protected := map[string]any{"alg": "ES256", "nonce": nonce, "url": url}
	if kid != "" {
		protected["kid"] = kid
	} else {
		protected["jwk"] = jwk(&key.PublicKey)
	}
	ph, err := json.Marshal(protected)
	if err != nil {
		return nil, err
	}
	// POST-as-GET sends an empty payload, not "{}" nor base64url of "{}".
	pl := ""
	if payload != nil {
		pl = b64(payload)
	}
	input := b64(ph) + "." + pl
	sum := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return nil, err
	}
	// ES256 is r||s with each half padded to 32 bytes, never ASN.1 DER:
	// a DER signature is what ecdsa.SignASN1 gives and what the CA rejects.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return json.Marshal(map[string]string{
		"protected": b64(ph),
		"payload":   pl,
		"signature": b64(sig),
	})
}

// VerifyES256 checks a raw r||s signature over the JWS signing input. The
// fake CA in acmetest uses it, and keeping it next to the signer means the
// encoder and the decoder share one notion of the format.
func VerifyES256(pub *ecdsa.PublicKey, signingInput string, sig []byte) bool {
	if len(sig) != 64 {
		return false
	}
	sum := sha256.Sum256([]byte(signingInput))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	return ecdsa.Verify(pub, sum[:], r, s)
}

// ParseJWK reads the public key out of a "jwk" header member; the inverse
// of what signJWS writes, used by the fake CA.
func ParseJWK(m map[string]string) (*ecdsa.PublicKey, error) {
	if m["kty"] != "EC" || m["crv"] != "P-256" {
		return nil, fmt.Errorf("acme: unsupported jwk %s/%s", m["kty"], m["crv"])
	}
	x, err := base64.RawURLEncoding.DecodeString(m["x"])
	if err != nil {
		return nil, err
	}
	y, err := base64.RawURLEncoding.DecodeString(m["y"])
	if err != nil {
		return nil, err
	}
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
		return nil, errors.New("acme: jwk point is not on the curve")
	}
	return pub, nil
}

// GenerateKey makes a P-256 key, usable both as an account key and as a
// certificate key.
func GenerateKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// MarshalKeyPEM encodes a key as "EC PRIVATE KEY", the form openssl and
// every other tool read without arguments.
func MarshalKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// ParseKeyPEM reads what MarshalKeyPEM wrote, and also PKCS#8, so a key an
// operator generated elsewhere can be dropped in.
func ParseKeyPEM(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("acme: no PEM block in key")
	}
	switch block.Type {
	case "EC PRIVATE KEY":
		return x509.ParseECPrivateKey(block.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		ec, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("acme: key is %T, want ECDSA", k)
		}
		return ec, nil
	}
	return nil, fmt.Errorf("acme: unsupported key type %q", block.Type)
}

// KeyAuthorization is token.thumbprint, what both challenge types serve.
func KeyAuthorization(token string, pub *ecdsa.PublicKey) (string, error) {
	tp, err := Thumbprint(pub)
	if err != nil {
		return "", err
	}
	return token + "." + tp, nil
}
