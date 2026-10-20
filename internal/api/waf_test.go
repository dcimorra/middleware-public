package api

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/traffic"
	"github.com/dcimorra/middleware-public/internal/waf"
)

func wafFixture(t *testing.T) (*fixture, *waf.WAF) {
	t.Helper()
	f := newFixture(t).withToken()
	w, err := waf.New(waf.Options{DataDir: filepath.Join(f.root, "data"), Bus: f.bus})
	if err != nil {
		t.Fatal(err)
	}
	rec := traffic.New(traffic.Options{})
	rec.Record("x.test", 200, 10, 0)
	f.srv.RegisterWAF(WAFOptions{WAF: w, Traffic: rec})
	return f, w
}

func TestTheWAFRoutesBlockUnblockAndChangeThePolicy(t *testing.T) {
	f, w := wafFixture(t)
	resp, body := f.call("GET", "/api/v1/waf", nil)
	if resp.StatusCode != 200 || body["global"] == nil {
		t.Fatalf("get: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("POST", "/api/v1/waf/block", map[string]string{"ip": "203.0.113.9", "duration": "1h", "reason": "test"})
	if resp.StatusCode != 200 {
		t.Fatalf("block: %d %v", resp.StatusCode, body)
	}
	if !w.IsBlocked(net.ParseIP("203.0.113.9")) {
		t.Fatal("not blocked")
	}
	resp, _ = f.call("POST", "/api/v1/waf/block", map[string]string{"ip": "not-an-ip"})
	if resp.StatusCode != 400 {
		t.Fatalf("bad ip: %d", resp.StatusCode)
	}
	resp, _ = f.call("DELETE", "/api/v1/waf/block/203.0.113.9", nil)
	if resp.StatusCode != 200 || w.IsBlocked(net.ParseIP("203.0.113.9")) {
		t.Fatalf("unblock: %d", resp.StatusCode)
	}
	resp, body = f.call("PUT", "/api/v1/waf", map[string]any{"max_body": "1m", "trips": 3})
	if resp.StatusCode != 200 || body["max_body"] != "1m" {
		t.Fatalf("put: %d %v", resp.StatusCode, body)
	}
	if w.Policy().Trips != 3 {
		t.Fatal("policy not applied")
	}
	resp, _ = f.call("PUT", "/api/v1/waf", map[string]any{"max_body": "lots"})
	if resp.StatusCode != 400 {
		t.Fatalf("invalid policy: %d", resp.StatusCode)
	}
	resp, _ = f.call("POST", "/api/v1/waf/under-attack", map[string]any{"enabled": true, "for": "2h", "bits": 18})
	g := w.GlobalAttack()
	if resp.StatusCode != 200 || !g.On || g.Bits != 18 || g.Until == "" {
		t.Fatalf("under attack: %d %+v", resp.StatusCode, g)
	}
	if until, _ := time.Parse(time.RFC3339, g.Until); until.Before(time.Now().Add(119*time.Minute)) || until.After(time.Now().Add(121*time.Minute)) {
		t.Fatalf("for 2h gave until %s", g.Until)
	}
	// Changing the difficulty leaves the deadline alone; "for": "" makes
	// it permanent.
	f.call("POST", "/api/v1/waf/under-attack", map[string]any{"enabled": true, "bits": 12})
	if g2 := w.GlobalAttack(); g2.Until != g.Until || g2.Bits != 12 {
		t.Fatalf("the deadline moved or the bits did not: %+v", g2)
	}
	f.call("POST", "/api/v1/waf/under-attack", map[string]any{"enabled": true, "for": ""})
	if w.GlobalAttack().Until != "" {
		t.Fatal("for \"\" did not make it permanent")
	}
	resp, _ = f.call("POST", "/api/v1/waf/under-attack", map[string]any{"enabled": true, "bits": 99})
	if resp.StatusCode != 400 {
		t.Fatalf("99 bits: %d", resp.StatusCode)
	}
	resp, _ = f.call("POST", "/api/v1/waf/under-attack", map[string]any{"enabled": true, "domain": "nope.test"})
	if resp.StatusCode != 500 && resp.StatusCode != 404 {
		t.Fatalf("under attack for an unknown site must fail: %d", resp.StatusCode)
	}
	resp, body = f.call("GET", "/api/v1/waf/traffic", nil)
	if resp.StatusCode != 200 || body["requests"] != float64(1) {
		t.Fatalf("traffic: %d %v", resp.StatusCode, body)
	}
}

func TestTheWAFRoutesSay503WhenTheWAFDidNotStart(t *testing.T) {
	f := newFixture(t).withToken()
	f.srv.RegisterWAF(WAFOptions{})
	resp, _ := f.call("GET", "/api/v1/waf", nil)
	if resp.StatusCode != 503 {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
}
