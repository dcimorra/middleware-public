package domains

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRDAPSaysFreeOrTakenAndDNSIsNotSure(t *testing.T) {
	var boots atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/dns.json":
			boots.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"services": [][][]string{{{"com", "net"}, {srv.URL + "/rdap"}}}})
		case r.URL.Path == "/rdap/domain/taken.com":
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "tld-prices.json"), []byte(`{"updated":"2026-01-02","usd":{"com":10.44}}`), 0o644)
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	s := New(Options{DataDir: dir, Bootstrap: srv.URL + "/dns.json", Now: func() time.Time { return now },
		LookupNS: func(_ context.Context, name string) ([]*net.NS, error) {
			if name == "taken.es" {
				return []*net.NS{{Host: "ns1.example."}}, nil
			}
			return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
		}})
	got := map[string]Result{}
	for _, name := range []string{"taken", "fresh"} {
		a, err := s.Search(context.Background(), name, []string{"com", ".es"})
		if err != nil {
			t.Fatal(err)
		}
		if !a.StalePrices || a.PricesFrom != "2026-01-02" {
			t.Fatalf("prices from %q, stale %v", a.PricesFrom, a.StalePrices)
		}
		for _, r := range a.Results {
			got[r.Domain] = r
		}
	}
	want := map[string]Result{
		"taken.com": {Status: "taken", Sure: true, Price: 10.44},
		"fresh.com": {Status: "free", Sure: true, Price: 10.44},
		"taken.es":  {Status: "taken", Sure: true},
		"fresh.es":  {Status: "free", Sure: false},
	}
	for d, w := range want {
		g := got[d]
		if g.Status != w.Status || g.Sure != w.Sure || g.Price != w.Price {
			t.Errorf("%s: %+v, want %+v", d, g, w)
		}
	}
	if boots.Load() != 1 {
		t.Fatalf("IANA's map was fetched %d times, want once a day", boots.Load())
	}
	if _, err := s.Search(context.Background(), "two.labels", []string{"com"}); err == nil {
		t.Fatal("a dotted name was searched")
	}
	if _, err := s.Search(context.Background(), strings.Repeat("a", 64), []string{"com"}); err == nil {
		t.Fatal("a 64-letter label was searched")
	}
}

// TestTheRealRDAP asks IANA and the real registries:
// DOMAINS_REAL=1 go test ./internal/domains -run Real -v
func TestTheRealRDAP(t *testing.T) {
	if os.Getenv("DOMAINS_REAL") == "" {
		t.Skip("DOMAINS_REAL=1 to ask the real registries")
	}
	s := New(Options{DataDir: t.TempDir()})
	for _, name := range []string{"google", "zq7v-mw-not-registered-4821"} {
		a, err := s.Search(context.Background(), name, []string{"com", "org", "es"})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range a.Results {
			t.Logf("%-40s %-8s sure=%v %s", r.Domain, r.Status, r.Sure, r.Error)
		}
	}
}
