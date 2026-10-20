package edge

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func from(remote string, h map[string]string) *http.Request {
	r := httptest.NewRequest("GET", "http://a.example/", nil)
	r.RemoteAddr = remote
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return r
}

func TestADirectClientCannotNameItsOwnAddress(t *testing.T) {
	r := from("203.0.113.9:5000", map[string]string{"CF-Connecting-IP": "1.2.3.4", "X-Forwarded-For": "5.6.7.8", "CF-IPCountry": "ES"})
	if ip := ClientIP(r); ip.String() != "203.0.113.9" {
		t.Fatalf("believed a forged header: %s", ip)
	}
	if c := Country(r); c != "" {
		t.Fatalf("believed a forged country: %q", c)
	}
	out := r.Header.Clone()
	Scrub(r, out)
	if out.Get("CF-IPCountry") != "" || out.Get("X-Forwarded-For") != "" {
		t.Fatalf("forged edge headers reached the site: %v", out)
	}
	if out.Get("CF-Connecting-IP") != "203.0.113.9" || out.Get("X-Real-IP") != "203.0.113.9" {
		t.Fatalf("the site was not told the real address: %v", out)
	}
}

func TestTheTunnelIsBelievedAboutTheClient(t *testing.T) {
	r := from("127.0.0.1:5000", map[string]string{"CF-Connecting-IP": "198.51.100.4", "CF-IPCountry": "pt"})
	if ip := ClientIP(r); ip.String() != "198.51.100.4" {
		t.Fatalf("got %s", ip)
	}
	if c := Country(r); c != "PT" {
		t.Fatalf("got %q", c)
	}
	out := r.Header.Clone()
	Scrub(r, out)
	if out.Get("CF-IPCountry") != "pt" || out.Get("CF-Connecting-IP") != "198.51.100.4" {
		t.Fatalf("the edge's headers were lost: %v", out)
	}
}

func TestAForwardedProtoIsOnlyBelievedFromTheEdge(t *testing.T) {
	if HTTPS(from("203.0.113.9:1", map[string]string{"X-Forwarded-Proto": "https"})) {
		t.Fatal("a direct client made itself https")
	}
	if !HTTPS(from("127.0.0.1:1", map[string]string{"X-Forwarded-Proto": "https"})) {
		t.Fatal("the tunnel's https was ignored")
	}
}

func TestClientIPTrustsHeadersOnlyFromLoopback(t *testing.T) {
	cases := []struct {
		remote string
		hdr    map[string]string
		want   string
	}{
		{"203.0.113.1:1", nil, "203.0.113.1"},
		{"203.0.113.1:1", map[string]string{"X-Forwarded-For": "1.2.3.4"}, "203.0.113.1"},
		{"203.0.113.1:1", map[string]string{"CF-Connecting-IP": "1.2.3.4"}, "203.0.113.1"},
		{"127.0.0.1:1", map[string]string{"CF-Connecting-IP": "1.2.3.4"}, "1.2.3.4"},
		{"127.0.0.1:1", map[string]string{"X-Forwarded-For": "9.9.9.9, 1.2.3.4"}, "1.2.3.4"},
		{"127.0.0.1:1", map[string]string{"X-Forwarded-For": "9.9.9.9, 127.0.0.1"}, "9.9.9.9"},
		{"[::1]:1", map[string]string{"X-Forwarded-For": "2001:db8::5"}, "2001:db8::5"},
		{"127.0.0.1:1", nil, "127.0.0.1"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		r.RemoteAddr = tc.remote
		for k, v := range tc.hdr {
			r.Header.Set(k, v)
		}
		if got := ClientIP(r).String(); got != tc.want {
			t.Errorf("%s %v: got %s, want %s", tc.remote, tc.hdr, got, tc.want)
		}
	}
}
