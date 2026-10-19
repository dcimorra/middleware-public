package api

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/acme"
	"github.com/dcimorra/middleware-public/internal/certs"
	"github.com/dcimorra/middleware-public/internal/inbound"
)

// fakeDNS records what the move asked it to publish.
type fakeDNS struct {
	enabled   bool
	published []string
}

func (d *fakeDNS) Enabled() bool { return d.enabled }
func (d *fakeDNS) Describe(_ context.Context, domains []string) (map[string]DNSInfo, error) {
	out := map[string]DNSInfo{}
	for _, dom := range domains {
		out[dom] = DNSInfo{DNS: "CNAME tunnel.example", TunnelRoute: true}
	}
	return out, nil
}
func (d *fakeDNS) Publish(_ context.Context, domain, door, ip string) error {
	d.published = append(d.published, domain+" "+door+" "+ip)
	return nil
}

func inboundFixture(t *testing.T, dns *fakeDNS) (*fixture, *inbound.Hook) {
	t.Helper()
	f := newFixture(t).withToken()
	store := certs.New(filepath.Join(f.root, "data", "certs"), nil)
	h := inbound.New(f.root, store, acme.NewHTTP01())
	h.Sites = func() []string { return []string{"x.test"} }
	f.srv.RegisterInbound(InboundOptions{Hook: h, DNS: dns})
	return f, h
}

func TestInboundStatusCarriesTheDNSColumnsWhenTheProviderIsOn(t *testing.T) {
	f, _ := inboundFixture(t, &fakeDNS{enabled: true})
	resp, body := f.call("GET", "/api/v1/inbound", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d %s", resp.StatusCode, text(body))
	}
	doors := body["doors"].(map[string]any)
	if doors["tunnel"].(map[string]any)["open"] != true {
		t.Fatalf("fresh state: tunnel must be open: %v", doors)
	}
	rows := body["domains"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows: %v", rows)
	}
	row := rows[0].(map[string]any)
	if row["domain"] != "x.test" || row["dns"] != "CNAME tunnel.example" || row["tunnel_route"] != "yes" {
		t.Fatalf("row: %v", row)
	}
}

func TestInboundStatusLeavesTheDNSColumnsEmptyWhenTheProviderIsOff(t *testing.T) {
	f, _ := inboundFixture(t, &fakeDNS{enabled: false})
	_, body := f.call("GET", "/api/v1/inbound", nil)
	row := body["domains"].([]any)[0].(map[string]any)
	if row["dns"] != "" || row["tunnel_route"] != "" {
		t.Fatalf("columns must be empty: %v", row)
	}
}

func TestMovingToTheTunnelMarksTheDomainAndPublishesDNS(t *testing.T) {
	dns := &fakeDNS{enabled: true}
	f, h := inboundFixture(t, dns)
	resp, body := f.call("POST", "/api/v1/inbound/move", map[string]any{"domains": []string{"x.test", "nope.test"}, "to": "tunnel"})
	if resp.StatusCode != 200 {
		t.Fatalf("move: %d %s", resp.StatusCode, text(body))
	}
	if body["ok"] != false {
		t.Fatalf("a domain that is not a site must fail the move: %v", body)
	}
	if h.State().DoorOf("x.test") != inbound.Tunnel {
		t.Fatal("x.test was not marked")
	}
	if len(dns.published) != 1 || dns.published[0] != "x.test tunnel " {
		t.Fatalf("published: %v (only the marked domain, and only once)", dns.published)
	}
	resp, _ = f.call("POST", "/api/v1/inbound/move", map[string]any{"domains": []string{"x.test"}, "to": "garage"})
	if resp.StatusCode != 400 {
		t.Fatalf("unknown door: %d", resp.StatusCode)
	}
}

func TestMovingToThePortsDoorWithoutAnIPSkipsTheDNSStepAndSaysWhy(t *testing.T) {
	dns := &fakeDNS{enabled: true}
	f, h := inboundFixture(t, dns)
	// Without an issuer the certificate cannot be obtained and the domain
	// must not be marked, let alone published.
	resp, body := f.call("POST", "/api/v1/inbound/move", map[string]any{"domains": []string{"x.test"}, "to": "ports"})
	if resp.StatusCode != 200 || body["ok"] != false {
		t.Fatalf("move without issuer: %d %v", resp.StatusCode, body)
	}
	if h.State().DoorOf("x.test") == inbound.Ports || len(dns.published) != 0 {
		t.Fatalf("a domain without a certificate must not be marked or published: %v", dns.published)
	}
	if !strings.Contains(text(body), "certificate not obtained") {
		t.Fatalf("lines must say why: %s", text(body))
	}
}

func TestDoorsOpenAndCloseThroughTheAPI(t *testing.T) {
	f, h := inboundFixture(t, nil)
	resp, body := f.call("POST", "/api/v1/inbound/doors/tunnel", map[string]any{"open": false})
	if resp.StatusCode != 200 {
		t.Fatalf("close: %d %s", resp.StatusCode, text(body))
	}
	if h.State().Tunnel.Open {
		t.Fatal("tunnel still open")
	}
	if st, err := inbound.LoadState(f.root); err != nil || st.Tunnel.Open {
		t.Fatalf("the decision must be on disk: %v %v", st, err)
	}
	resp, _ = f.call("POST", "/api/v1/inbound/doors/window", map[string]any{"open": true})
	if resp.StatusCode != 400 {
		t.Fatalf("unknown door: %d", resp.StatusCode)
	}
	resp, body = f.call("GET", "/api/v1/inbound/certificates", nil)
	if resp.StatusCode != 200 || strings.TrimSpace(text(body)) != "[]" {
		t.Fatalf("certificates: %d %q", resp.StatusCode, text(body))
	}
	resp, _ = f.call("POST", "/api/v1/inbound/certificates/x.test/renew", nil)
	if resp.StatusCode != 502 {
		t.Fatalf("renew without an issuer must be 502, got %d", resp.StatusCode)
	}
}
