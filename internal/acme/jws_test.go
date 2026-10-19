package acme

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestANewAccountJWSEmbedsTheKeyAndSignsWithRawES256(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	out, err := signJWS(key, "https://ca.test/new-account", "nonce-1", "", []byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var env struct{ Protected, Payload, Signature string }
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	ph, _ := base64.RawURLEncoding.DecodeString(env.Protected)
	var h map[string]any
	if err := json.Unmarshal(ph, &h); err != nil {
		t.Fatal(err)
	}
	if h["alg"] != "ES256" || h["nonce"] != "nonce-1" || h["url"] != "https://ca.test/new-account" {
		t.Fatalf("protected header: %v", h)
	}
	if _, ok := h["kid"]; ok {
		t.Fatal("a new account must not name a kid")
	}
	jwk, ok := h["jwk"].(map[string]any)
	if !ok || jwk["kty"] != "EC" || jwk["crv"] != "P-256" {
		t.Fatalf("jwk: %v", h["jwk"])
	}
	sig, _ := base64.RawURLEncoding.DecodeString(env.Signature)
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want raw r||s of 64", len(sig))
	}
	if !VerifyES256(&key.PublicKey, env.Protected+"."+env.Payload, sig) {
		t.Fatal("signature does not verify")
	}
	if strings.Contains(string(out), "=") {
		t.Fatal("base64url must not be padded")
	}
}

func TestAPostAsGetHasAnEmptyPayloadAndNamesTheAccount(t *testing.T) {
	key, _ := GenerateKey()
	out, err := signJWS(key, "https://ca.test/order/1", "n", "https://ca.test/acct/7", nil)
	if err != nil {
		t.Fatal(err)
	}
	var env struct{ Protected, Payload string }
	_ = json.Unmarshal(out, &env)
	if env.Payload != "" {
		t.Fatalf("payload %q, want empty", env.Payload)
	}
	ph, _ := base64.RawURLEncoding.DecodeString(env.Protected)
	if !strings.Contains(string(ph), `"kid":"https://ca.test/acct/7"`) || strings.Contains(string(ph), "jwk") {
		t.Fatalf("protected: %s", ph)
	}
}

func TestTheThumbprintSurvivesAJWKRoundTrip(t *testing.T) {
	key, _ := GenerateKey()
	pub, err := ParseJWK(jwk(&key.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Thumbprint(&key.PublicKey)
	b, _ := Thumbprint(pub)
	if a != b || len(a) != 43 {
		t.Fatalf("thumbprints %q and %q", a, b)
	}
}

func TestAKeyRoundTripsThroughPEM(t *testing.T) {
	key, _ := GenerateKey()
	data, err := MarshalKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseKeyPEM(data)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(key) {
		t.Fatal("key changed through PEM")
	}
	if _, err := ParseKeyPEM([]byte("garbage")); err == nil {
		t.Fatal("garbage parsed")
	}
}

func TestANonProblemBodyStillBecomesAProblem(t *testing.T) {
	resp := &http.Response{StatusCode: 502, Header: http.Header{"Content-Type": {"text/html"}, "Retry-After": {"12"}}}
	p := parseProblem(resp, []byte("<html>bad gateway</html>"))
	if p.Status != 502 || p.Detail != "<html>bad gateway</html>" || p.RetryAfter != 12*time.Second {
		t.Fatalf("%+v", p)
	}
	resp = &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": {"application/problem+json"}}}
	p = parseProblem(resp, []byte(`{"type":"urn:ietf:params:acme:error:rateLimited","detail":"slow down","status":429}`))
	if !p.RateLimited() || !p.IsCode("rateLimited") || p.Error() != "acme: rateLimited (429): slow down" {
		t.Fatalf("%q", p.Error())
	}
}
