// Package webauthn checks passkeys (WebAuthn level 2) against the
// standard library: a registration gives a credential to keep, a login
// proves the browser holds its private key. Written by hand because the
// core takes no dependencies; what it accepts is narrow on purpose.
//
// What it checks, and why each one:
//   - the client data's type, challenge and origin: the browser writes the
//     origin it was on, so a copy of the panel at another address cannot
//     get a signature that is valid here;
//   - the RP ID hash in the authenticator data: the key was made for this
//     name and no other;
//   - user presence: a touch, a fingerprint, a PIN;
//   - the signature over authenticator data and the client data's hash;
//   - the signature counter, when the authenticator keeps one: a counter
//     that goes backwards means a cloned key.
//
// Attestation (which make of authenticator it is) is not checked: this is
// one operator's console, not a bank choosing hardware, and asking for it
// makes browsers show an extra privacy prompt.
package webauthn

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

// Credential is what is kept of a registered passkey.
type Credential struct {
	ID        []byte `json:"id"`
	PublicKey []byte `json:"public_key"` // COSE_Key as the authenticator sent it
	Alg       int    `json:"alg"`
	SignCount uint32 `json:"sign_count"`
}

// Expect is what the server asked for and where.
type Expect struct {
	Challenge []byte
	// Origin is the panel's, scheme and host (and port when not default),
	// exactly as the browser writes it.
	Origin string
	// RPID is the host name the passkey is bound to.
	RPID string
}

var b64 = base64.RawURLEncoding

type clientData struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	Origin    string `json:"origin"`
}

func checkClient(raw []byte, typ string, e Expect) error {
	var c clientData
	if err := json.Unmarshal(raw, &c); err != nil {
		return fmt.Errorf("client data: %w", err)
	}
	if c.Type != typ {
		return fmt.Errorf("client data: type %q, want %q", c.Type, typ)
	}
	got, err := b64.DecodeString(c.Challenge)
	if err != nil || len(e.Challenge) == 0 || !bytes.Equal(got, e.Challenge) {
		return errors.New("client data: the challenge is not the one issued")
	}
	if c.Origin != e.Origin {
		return fmt.Errorf("client data: origin %q, want %q", c.Origin, e.Origin)
	}
	return nil
}

const (
	flagUP = 0x01 // user present
	flagUV = 0x04 // user verified
	flagAT = 0x40 // attested credential data included
)

type authData struct {
	rpIDHash  []byte
	flags     byte
	signCount uint32
	credID    []byte
	cose      []byte
}

func parseAuthData(b []byte, wantCred bool) (authData, error) {
	if len(b) < 37 {
		return authData{}, errors.New("authenticator data: too short")
	}
	a := authData{rpIDHash: b[:32], flags: b[32], signCount: binary.BigEndian.Uint32(b[33:37])}
	if !wantCred {
		return a, nil
	}
	if a.flags&flagAT == 0 {
		return authData{}, errors.New("authenticator data: no credential in it")
	}
	rest := b[37:]
	if len(rest) < 18 {
		return authData{}, errors.New("authenticator data: credential too short")
	}
	n := int(binary.BigEndian.Uint16(rest[16:18]))
	rest = rest[18:]
	if n == 0 || n > 1023 || len(rest) < n {
		return authData{}, errors.New("authenticator data: bad credential id")
	}
	a.credID, rest = rest[:n], rest[n:]
	_, after, err := decode(rest)
	if err != nil {
		return authData{}, fmt.Errorf("authenticator data: public key: %w", err)
	}
	a.cose = rest[:len(rest)-len(after)]
	return a, nil
}

func checkRP(a authData, rpID string) error {
	sum := sha256.Sum256([]byte(rpID))
	if !bytes.Equal(a.rpIDHash, sum[:]) {
		return errors.New("authenticator data: made for another name")
	}
	if a.flags&flagUP == 0 {
		return errors.New("authenticator data: no user presence")
	}
	return nil
}

// Register checks a navigator.credentials.create() answer and returns the
// credential to keep.
func Register(clientDataJSON, attestationObject []byte, e Expect) (Credential, error) {
	if err := checkClient(clientDataJSON, "webauthn.create", e); err != nil {
		return Credential{}, err
	}
	v, rest, err := decode(attestationObject)
	if err != nil || len(rest) != 0 {
		return Credential{}, errors.New("attestation object: malformed")
	}
	m, ok := v.(map[any]any)
	if !ok {
		return Credential{}, errors.New("attestation object: not a map")
	}
	raw, ok := m["authData"].([]byte)
	if !ok {
		return Credential{}, errors.New("attestation object: no authenticator data")
	}
	a, err := parseAuthData(raw, true)
	if err != nil {
		return Credential{}, err
	}
	if err := checkRP(a, e.RPID); err != nil {
		return Credential{}, err
	}
	_, alg, err := publicKey(a.cose)
	if err != nil {
		return Credential{}, err
	}
	return Credential{ID: a.credID, PublicKey: a.cose, Alg: alg, SignCount: a.signCount}, nil
}

// ErrCloned is a signature counter that went backwards.
var ErrCloned = errors.New("the passkey's counter went backwards: it may have been copied")

// Login checks a navigator.credentials.get() answer for cred and returns
// the counter to keep.
func Login(cred Credential, clientDataJSON, authenticatorData, signature []byte, e Expect) (uint32, error) {
	if err := checkClient(clientDataJSON, "webauthn.get", e); err != nil {
		return 0, err
	}
	a, err := parseAuthData(authenticatorData, false)
	if err != nil {
		return 0, err
	}
	if err := checkRP(a, e.RPID); err != nil {
		return 0, err
	}
	sum := sha256.Sum256(clientDataJSON)
	signed := append(append([]byte(nil), authenticatorData...), sum[:]...)
	if err := verify(cred.PublicKey, signed, signature); err != nil {
		return 0, err
	}
	// Synced passkeys keep the counter at zero; only a counter that
	// exists and goes backwards says anything.
	if a.signCount != 0 || cred.SignCount != 0 {
		if a.signCount <= cred.SignCount {
			return 0, ErrCloned
		}
	}
	return a.signCount, nil
}
