package webauthn

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"
)

// enc is a tiny CBOR encoder for the tests: what an authenticator would
// send, built from keys made here.
func enc(v any) []byte {
	head := func(major byte, n int) []byte {
		switch {
		case n < 24:
			return []byte{major<<5 | byte(n)}
		case n < 256:
			return []byte{major<<5 | 24, byte(n)}
		default:
			b := []byte{major<<5 | 25, 0, 0}
			binary.BigEndian.PutUint16(b[1:], uint16(n))
			return b
		}
	}
	switch x := v.(type) {
	case int:
		if x >= 0 {
			return head(0, x)
		}
		return head(1, -1-x)
	case []byte:
		return append(head(2, len(x)), x...)
	case string:
		return append(head(3, len(x)), x...)
	case [][2]any: // an ordered map
		out := head(5, len(x))
		for _, kv := range x {
			out = append(out, enc(kv[0])...)
			out = append(out, enc(kv[1])...)
		}
		return out
	}
	panic("enc")
}

type key struct {
	cose []byte
	sign func([]byte) []byte
}

func es256() key {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	x, y := make([]byte, 32), make([]byte, 32)
	k.X.FillBytes(x)
	k.Y.FillBytes(y)
	return key{
		cose: enc([][2]any{{1, 2}, {3, -7}, {-1, 1}, {-2, x}, {-3, y}}),
		sign: func(d []byte) []byte {
			s := sha256.Sum256(d)
			sig, _ := ecdsa.SignASN1(rand.Reader, k, s[:])
			return sig
		},
	}
}

func ed() key {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	return key{
		cose: enc([][2]any{{1, 1}, {3, -8}, {-1, 6}, {-2, []byte(pub)}}),
		sign: func(d []byte) []byte { return ed25519.Sign(priv, d) },
	}
}

var site = Expect{Challenge: []byte("challenge-0123456789"), Origin: "https://panel.example", RPID: "panel.example"}

func client(typ string, e Expect) []byte {
	b, _ := json.Marshal(map[string]string{"type": typ, "challenge": b64.EncodeToString(e.Challenge), "origin": e.Origin})
	return b
}

func makeAuthData(rpID string, flags byte, count uint32, credID, cose []byte) []byte {
	h := sha256.Sum256([]byte(rpID))
	b := append(h[:], flags, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(b[33:], count)
	if credID != nil {
		b = append(b, make([]byte, 16)...) // aaguid
		b = append(b, byte(len(credID)>>8), byte(len(credID)))
		b = append(b, credID...)
		b = append(b, cose...)
	}
	return b
}

func register(t *testing.T, k key) Credential {
	t.Helper()
	att := enc([][2]any{{"fmt", "none"}, {"attStmt", [][2]any{}}, {"authData", makeAuthData(site.RPID, flagUP|flagUV|flagAT, 0, []byte("cred-1"), k.cose)}})
	c, err := Register(client("webauthn.create", site), att, site)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func login(c Credential, k key, e Expect, rpID string, flags byte, count uint32) (uint32, error) {
	cd := client("webauthn.get", e)
	ad := makeAuthData(rpID, flags, count, nil, nil)
	sum := sha256.Sum256(cd)
	return Login(c, cd, ad, k.sign(append(append([]byte(nil), ad...), sum[:]...)), site)
}

func TestAPasskeyRegistersAndLogsIn(t *testing.T) {
	for name, k := range map[string]key{"ES256": es256(), "Ed25519": ed()} {
		c := register(t, k)
		if string(c.ID) != "cred-1" {
			t.Fatalf("%s: id %q", name, c.ID)
		}
		if _, err := login(c, k, site, site.RPID, flagUP, 0); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestWhatALoginMustNotGetPast(t *testing.T) {
	k := es256()
	c := register(t, k)
	other := Expect{Challenge: site.Challenge, Origin: "https://evil.example", RPID: site.RPID}
	if _, err := login(c, k, other, site.RPID, flagUP, 0); err == nil {
		t.Error("another origin logged in")
	}
	stale := Expect{Challenge: []byte("an old challenge"), Origin: site.Origin, RPID: site.RPID}
	if _, err := login(c, k, stale, site.RPID, flagUP, 0); err == nil {
		t.Error("another challenge logged in")
	}
	if _, err := login(c, k, site, "evil.example", flagUP, 0); err == nil {
		t.Error("a key made for another name logged in")
	}
	if _, err := login(c, k, site, site.RPID, 0, 0); err == nil {
		t.Error("no user presence logged in")
	}
	if _, err := login(c, es256(), site, site.RPID, flagUP, 0); err == nil {
		t.Error("another key's signature logged in")
	}
	// A counter: going forward is fine, backwards is a copy.
	c.SignCount = 5
	if n, err := login(c, k, site, site.RPID, flagUP, 6); err != nil || n != 6 {
		t.Errorf("counter forward: %d %v", n, err)
	}
	if _, err := login(c, k, site, site.RPID, flagUP, 5); !errors.Is(err, ErrCloned) {
		t.Errorf("counter backwards: %v", err)
	}
	// The registration's client data cannot be replayed as a login.
	cd := client("webauthn.create", site)
	if _, err := Login(c, cd, makeAuthData(site.RPID, flagUP, 9, nil, nil), nil, site); err == nil {
		t.Error("a create answer logged in")
	}
}

func TestHostileCBORIsRefusedNotAllocated(t *testing.T) {
	for _, b := range [][]byte{
		{},
		{0x5b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, // a byte string of 2^64-1
		{0x9b, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00}, // an array of 2^32
		{0x5f},             // indefinite length
		{0xc6, 0x01},       // a tag
		{0xa1, 0x40, 0x01}, // a map with a byte-string key
	} {
		if _, _, err := decode(b); err == nil {
			t.Errorf("% x decoded", b)
		}
	}
	deep := make([]byte, 100)
	for i := range deep {
		deep[i] = 0x81
	}
	if _, _, err := decode(deep); err == nil {
		t.Error("100 nested arrays decoded")
	}
}

func FuzzDecode(f *testing.F) {
	f.Add(enc([][2]any{{1, 2}, {"a", []byte("b")}}))
	f.Fuzz(func(t *testing.T, b []byte) { _, _, _ = decode(b) })
}
