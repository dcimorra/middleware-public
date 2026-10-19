// Package wireguard is the part of WireGuard the middleware needs without
// importing anything: keys, the two config files, address allocation inside
// the VPN subnet and the parser for `wg show dump`. The kernel module and
// wireguard-tools do the actual networking; this package only produces and
// reads what they consume.
package wireguard

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// KeySize is the length of every WireGuard key: Curve25519 scalars and
// points, and preshared keys, are all 32 bytes.
const KeySize = 32

// Key is a 32-byte WireGuard key. Its text form is standard base64 with
// padding, 44 characters, which is exactly what wg(8) prints and expects.
type Key [KeySize]byte

// ErrBadKey is returned when a key string is not 32 bytes of base64.
var ErrBadKey = errors.New("wireguard: key must be 32 bytes in base64")

// ParseKey decodes the 44-character base64 form.
func ParseKey(s string) (Key, error) {
	var k Key
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) != KeySize {
		return k, ErrBadKey
	}
	copy(k[:], raw)
	return k, nil
}

// String is the base64 form.
func (k Key) String() string { return base64.StdEncoding.EncodeToString(k[:]) }

// IsZero reports whether the key was never set.
func (k Key) IsZero() bool { return k == Key{} }

// MarshalText makes a Key a base64 string in JSON, so a stored peer list
// is readable with any editor and matches what wg prints.
func (k Key) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// UnmarshalText accepts the base64 form; an empty string is the zero key so
// optional keys (preshared) can be omitted.
func (k *Key) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		*k = Key{}
		return nil
	}
	parsed, err := ParseKey(string(b))
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

// GeneratePrivateKey returns a new private key clamped the way `wg genkey`
// clamps it. The clamping does not change the derived public key (X25519
// clamps again before multiplying) but it keeps the stored bytes identical
// to what wireguard-tools would have written, so a key we generated and a
// key the operator pasted are indistinguishable.
func GeneratePrivateKey() (Key, error) {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		return k, err
	}
	k[0] &= 248
	k[31] &= 127
	k[31] |= 64
	return k, nil
}

// GeneratePresharedKey returns 32 random bytes, as `wg genpsk` does.
func GeneratePresharedKey() (Key, error) {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		return k, err
	}
	return k, nil
}

// Public derives the public key, as `wg pubkey` does.
func (k Key) Public() (Key, error) {
	var pub Key
	priv, err := ecdh.X25519().NewPrivateKey(k[:])
	if err != nil {
		return pub, fmt.Errorf("wireguard: %w", err)
	}
	copy(pub[:], priv.PublicKey().Bytes())
	return pub, nil
}
