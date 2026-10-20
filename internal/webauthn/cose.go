package webauthn

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
)

// The COSE algorithms accepted (RFC 9053). What a passkey on a phone, a
// laptop or a security key offers: ES256 almost always, RS256 on Windows
// Hello, Ed25519 on some keys.
const (
	AlgES256 = -7
	AlgRS256 = -257
	AlgEdDSA = -8
)

// Algorithms is the list offered to the browser, preferred first.
var Algorithms = []int{AlgES256, AlgEdDSA, AlgRS256}

// publicKey reads a COSE_Key.
func publicKey(cose []byte) (crypto.PublicKey, int, error) {
	v, rest, err := decode(cose)
	if err != nil {
		return nil, 0, err
	}
	if len(rest) != 0 {
		return nil, 0, errors.New("cose: trailing bytes")
	}
	m, ok := v.(map[any]any)
	if !ok {
		return nil, 0, errors.New("cose: not a map")
	}
	kty, _ := m[int64(1)].(int64)
	alg, _ := m[int64(3)].(int64)
	switch {
	case kty == 2 && alg == AlgES256:
		crv, _ := m[int64(-1)].(int64)
		x, _ := m[int64(-2)].([]byte)
		y, _ := m[int64(-3)].([]byte)
		if crv != 1 || len(x) != 32 || len(y) != 32 {
			return nil, 0, errors.New("cose: ES256 needs a P-256 point")
		}
		pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
			return nil, 0, errors.New("cose: the point is not on P-256")
		}
		return pub, AlgES256, nil
	case kty == 3 && alg == AlgRS256:
		nb, _ := m[int64(-1)].([]byte)
		eb, _ := m[int64(-2)].([]byte)
		if len(nb) < 256 || len(eb) == 0 || len(eb) > 4 {
			return nil, 0, errors.New("cose: RS256 needs a 2048-bit key or larger")
		}
		e := 0
		for _, c := range eb {
			e = e<<8 | int(c)
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}, AlgRS256, nil
	case kty == 1 && alg == AlgEdDSA:
		crv, _ := m[int64(-1)].(int64)
		x, _ := m[int64(-2)].([]byte)
		if crv != 6 || len(x) != ed25519.PublicKeySize {
			return nil, 0, errors.New("cose: EdDSA needs an Ed25519 key")
		}
		return ed25519.PublicKey(x), AlgEdDSA, nil
	}
	return nil, 0, fmt.Errorf("cose: key type %d with algorithm %d is not accepted", kty, alg)
}

// verify checks sig over data with a COSE key.
func verify(cose, data, sig []byte) error {
	pub, alg, err := publicKey(cose)
	if err != nil {
		return err
	}
	ok := false
	switch alg {
	case AlgES256:
		sum := sha256.Sum256(data)
		ok = ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), sum[:], sig)
	case AlgRS256:
		sum := sha256.Sum256(data)
		ok = rsa.VerifyPKCS1v15(pub.(*rsa.PublicKey), crypto.SHA256, sum[:], sig) == nil
	case AlgEdDSA:
		ok = ed25519.Verify(pub.(ed25519.PublicKey), data, sig)
	}
	if !ok {
		return errors.New("the signature does not verify")
	}
	return nil
}
