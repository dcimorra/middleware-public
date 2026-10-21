package cloudflare

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/modules"
	"github.com/dcimorra/middleware-public/internal/secrets"
)

func TestFindZonePicksTheLongestSuffix(t *testing.T) {
	f := newFakeAPI()
	defer f.srv.Close()
	ctx := context.Background()
	c := f.client()
	z, err := c.FindZone(ctx, "api.shop.example.com")
	if err != nil || z.ID != "z2" {
		t.Fatalf("want shop.example.com, got %+v %v", z, err)
	}
	z, err = c.FindZone(ctx, "www.example.com")
	if err != nil || z.ID != "z1" {
		t.Fatalf("want example.com, got %+v %v", z, err)
	}
	if _, err := c.FindZone(ctx, "notexample.com"); err == nil {
		t.Fatal("a name that only ends with the zone's letters is not in the zone")
	}
}

func TestZonesAreListedAcrossPages(t *testing.T) {
	f := newFakeAPI()
	defer f.srv.Close()
	for i := 0; i < 120; i++ {
		f.zones = append(f.zones, Zone{ID: fmt.Sprintf("p%d", i), Name: fmt.Sprintf("z%03d.test", i)})
	}
	zones, err := f.client().Zones(context.Background())
	if err != nil || len(zones) != 122 {
		t.Fatalf("want every page, got %d %v", len(zones), err)
	}
}

func TestPublishingToTheTunnelWritesTheRouteBeforeTheCNAME(t *testing.T) {
	f := newFakeAPI()
	defer f.srv.Close()
	// The domain currently points at the machine directly with two A
	// records; moving it to the tunnel must leave exactly one CNAME.
	f.records["z1"] = []Record{{ID: "a1", Type: "A", Name: "www.example.com", Content: "1.2.3.4"}, {ID: "a2", Type: "A", Name: "www.example.com", Content: "1.2.3.5"}, {ID: "x", Type: "TXT", Name: "www.example.com", Content: "keep"}}
	c := f.client()
	if err := c.Publish(context.Background(), "www.example.com", "tunnel", ""); err != nil {
		t.Fatal(err)
	}
	want := []string{"ingress.set old.example.com,www.example.com", "dns.overwrite CNAME www.example.com tun1.cfargotunnel.com", "dns.delete A www.example.com"}
	if strings.Join(f.writes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("writes in the wrong order:\n%s\nwant:\n%s", strings.Join(f.writes, "\n"), strings.Join(want, "\n"))
	}
	var cnames, txts int
	for _, r := range f.records["z1"] {
		switch r.Type {
		case "CNAME":
			cnames++
			if !r.Proxied {
				t.Fatal("a tunnel CNAME must be proxied or Cloudflare refuses to route it")
			}
		case "TXT":
			txts++
		case "A":
			t.Fatal("no A record may remain beside the CNAME")
		}
	}
	if cnames != 1 || txts != 1 {
		t.Fatalf("records after publish: %+v", f.records["z1"])
	}
	if f.ingress[len(f.ingress)-1].Service != CatchAll {
		t.Fatal("the catch-all must stay last")
	}
	// Publishing again is a no-op on DNS and rewrites the same route.
	f.writes = nil
	if err := c.Publish(context.Background(), "www.example.com", "tunnel", ""); err != nil {
		t.Fatal(err)
	}
	for _, w := range f.writes {
		if strings.HasPrefix(w, "dns.") {
			t.Fatalf("a second publish must not touch DNS: %v", f.writes)
		}
	}
}

func TestPublishingToThePortsDoorWritesTheARecordBeforeRemovingTheRoute(t *testing.T) {
	f := newFakeAPI()
	defer f.srv.Close()
	f.records["z1"] = []Record{{ID: "c1", Type: "CNAME", Name: "www.example.com", Content: "tun1.cfargotunnel.com", Proxied: true}}
	f.ingress = []Ingress{{Hostname: "www.example.com", Service: "http://127.0.0.1:46100"}, {Service: CatchAll}}
	c := f.client()
	if err := c.Publish(context.Background(), "www.example.com", "ports", "203.0.113.7"); err != nil {
		t.Fatal(err)
	}
	want := []string{"dns.overwrite A www.example.com 203.0.113.7", "ingress.set "}
	if strings.Join(f.writes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("writes in the wrong order:\n%s", strings.Join(f.writes, "\n"))
	}
	if len(f.ingress) != 1 || f.ingress[0].Service != CatchAll {
		t.Fatalf("the route must be gone and the catch-all kept: %+v", f.ingress)
	}
	if err := c.Publish(context.Background(), "www.example.com", "ports", ""); err == nil {
		t.Fatal("the ports door without an ip must be refused")
	}
	if err := c.Publish(context.Background(), "www.example.com", "sideways", "1.1.1.1"); err == nil {
		t.Fatal("an unknown mode must be refused")
	}
}

func TestAMissingWritePermissionIsReportedByTheAPIError(t *testing.T) {
	f := newFakeAPI()
	defer f.srv.Close()
	f.denied["tunnel.write"] = true
	err := f.client().Publish(context.Background(), "www.example.com", "tunnel", "")
	if !IsForbidden(err) {
		t.Fatalf("want a forbidden error, got %v", err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("nothing must be written when the first step fails: %v", f.writes)
	}
}

func newModule(t *testing.T, f *fakeAPI) (*Module, *modules.Manager) {
	t.Helper()
	t.Setenv(secrets.EnvKey, "")
	mgr, err := modules.New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := New(mgr)
	m.BaseURL = f.srv.URL
	m.Service = "http://127.0.0.1:46100"
	mgr.Register(m)
	return m, mgr
}

func lineTexts(lines []modules.Line) string {
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, "[%s] %s\n", l.Status, l.Text)
	}
	return b.String()
}

func TestVerifyReportsOneLinePerPermissionAndNamesWhatItDoesNotProbe(t *testing.T) {
	f := newFakeAPI()
	defer f.srv.Close()
	m, _ := newModule(t, f)
	// The account's only tunnel is named, never taken: it may be another
	// server's.
	cfg := modules.Config{"token": "good-token"}
	lines := m.Verify(context.Background(), cfg)
	text := lineTexts(lines)
	if cfg["tunnel_id"] != "" {
		t.Fatalf("a tunnel was taken by itself: %v", cfg)
	}
	if !strings.Contains(text, "[skip] no tunnel chosen: DNS only. To route through a tunnel, set tunnel_id to one that is this server's: house (tun1)") {
		t.Fatalf("the tunnels were not offered:\n%s", text)
	}
	lines = m.Verify(context.Background(), modules.Config{"token": "good-token", "tunnel_id": "tun1"})
	text = lineTexts(lines)
	for _, want := range []string{
		"[ok] token is valid",
		"[ok] can list zones: 2 (example.com, shop.example.com)",
		"[ok] can read DNS records of example.com",
		"[ok] found account Main (acct1)",
		"[ok] can reach tunnel tun1 and read its 1 ingress rules",
		"[skip] write permissions",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if modules.Failed(lines) {
		t.Fatalf("nothing should fail:\n%s", text)
	}
}

func TestATokenWithoutDNSReadFailsThatLineAndIsNotSaved(t *testing.T) {
	f := newFakeAPI()
	defer f.srv.Close()
	f.denied["dns.read"] = true
	m, mgr := newModule(t, f)
	lines, err := mgr.Enable(context.Background(), Key, modules.Config{"token": "good-token"}, nil)
	if err != modules.ErrVerifyFailed {
		t.Fatalf("want ErrVerifyFailed, got %v\n%s", err, lineTexts(lines))
	}
	text := lineTexts(lines)
	if !strings.Contains(text, "[fail] cannot read DNS records of example.com") || !strings.Contains(text, "[ok] can list zones") {
		t.Fatalf("the failing permission must be named and the others still reported:\n%s", text)
	}
	if _, err := m.Client(); err == nil {
		t.Fatal("no credentials may be saved after a failed verify")
	}
	if _, err := os.Stat(filepath.Join(mgr.Root(), "data", modules.StoreFile)); err == nil {
		t.Fatal("the store must not exist")
	}
}

func TestAWrongTokenFailsOnTheFirstLine(t *testing.T) {
	f := newFakeAPI()
	defer f.srv.Close()
	m, _ := newModule(t, f)
	lines := m.Verify(context.Background(), modules.Config{"token": "wrong"})
	if len(lines) != 1 || lines[0].Status != modules.StatusFail {
		t.Fatalf("a bad token stops at the first line:\n%s", lineTexts(lines))
	}
}

func TestEnableInstallsTheConnectorOfTheChosenTunnelAndSavesTheAccount(t *testing.T) {
	f := newFakeAPI()
	defer f.srv.Close()
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/cloudflared-linux-amd64") && !strings.HasSuffix(r.URL.Path, "/cloudflared-linux-arm64") {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("#!/bin/sh\necho fake cloudflared\n"))
	}))
	defer release.Close()
	old := ReleaseURL
	ReleaseURL = release.URL + "/cloudflared-linux-%s"
	defer func() { ReleaseURL = old }()

	m, mgr := newModule(t, f)
	var steps []string
	lines, err := mgr.Enable(context.Background(), Key, modules.Config{"token": "good-token", "tunnel_id": "tun1"}, func(s string) { steps = append(steps, s) })
	if err != nil {
		t.Fatalf("%v\n%s", err, lineTexts(lines))
	}
	cfg, _ := mgr.Config(Key)
	if cfg["account_id"] != "acct1" || cfg["tunnel_id"] != "tun1" {
		t.Fatalf("discovered ids must be saved: %v", cfg)
	}
	if !mgr.Installed(Key) {
		t.Fatalf("the connector must be installed under modules/bin; steps: %v", steps)
	}
	root := mgr.Root()
	config, err := os.ReadFile(filepath.Join(root, "modules", "cloudflared", "config.yml"))
	if err != nil || !strings.Contains(string(config), "tunnel: tun1") {
		t.Fatalf("config.yml: %q %v", config, err)
	}
	info, err := os.Stat(filepath.Join(root, "modules", "cloudflared", "token"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the tunnel token must be written owner-only: %v %v", info, err)
	}
	c, err := m.Client()
	if err != nil || c.TunnelID != "tun1" || c.Service != "http://127.0.0.1:46100" {
		t.Fatalf("client from the store: %+v %v", c, err)
	}
	// The routes answer through the manager's handler once enabled.
	rec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/cloudflare/ingress", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "old.example.com") {
		t.Fatalf("ingress route: %d %s", rec.Code, rec.Body.String())
	}
}

func TestWithoutATunnelTheModuleIsDNSOnly(t *testing.T) {
	f := newFakeAPI()
	defer f.srv.Close()
	f.tunnels = nil
	_, mgr := newModule(t, f)
	lines, err := mgr.Enable(context.Background(), Key, modules.Config{"token": "good-token"}, nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, lineTexts(lines))
	}
	if !strings.Contains(lineTexts(lines), "[skip] no tunnel chosen: DNS only. The account has no tunnel") {
		t.Fatalf("the absence of a tunnel must be said:\n%s", lineTexts(lines))
	}
	if _, err := os.Stat(filepath.Join(mgr.Root(), "modules", "bin", BinaryName)); err == nil {
		t.Fatal("no connector without a tunnel")
	}
}

func TestUnpublishRemovesOnlyWhatPointsAtThisHouse(t *testing.T) {
	f := newFakeAPI()
	defer f.srv.Close()
	c := f.client()
	ctx := context.Background()
	if err := c.Publish(ctx, "a.example.com", "tunnel", ""); err != nil {
		t.Fatal(err)
	}
	// Someone else's record on another name, and a TXT on ours.
	f.records["z1"] = append(f.records["z1"],
		Record{ID: "o1", Type: "A", Name: "b.example.com", Content: "9.9.9.9"},
		Record{ID: "t1", Type: "TXT", Name: "a.example.com", Content: "keep"})
	f.writes = nil
	if err := c.Unpublish(ctx, "a.example.com", "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.HasIngress(ctx, "a.example.com"); ok {
		t.Fatal("the tunnel rule is still there")
	}
	for _, r := range f.records["z1"] {
		if r.Name == "a.example.com" && r.Type == "CNAME" {
			t.Fatal("the CNAME to the tunnel is still there")
		}
	}
	if len(f.records["z1"]) != 2 {
		t.Fatalf("records that were not ours were touched: %+v", f.records["z1"])
	}
	// A record pointing elsewhere is not removed, even on the same name.
	f.records["z1"] = append(f.records["z1"], Record{ID: "x1", Type: "A", Name: "c.example.com", Content: "5.5.5.5"})
	if err := c.Unpublish(ctx, "c.example.com", "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range f.records["z1"] {
		found = found || r.ID == "x1"
	}
	if !found {
		t.Fatal("a record pointing at another machine was deleted")
	}
	if ok, _ := c.Covers(ctx, "x.shop.example.com"); !ok {
		t.Fatal("a name in a known zone is not covered")
	}
	if ok, _ := c.Covers(ctx, "elsewhere.org"); ok {
		t.Fatal("a name in no known zone is covered")
	}
}

func TestAChallengeRecordIsRemovedAloneLeavingOtherTXT(t *testing.T) {
	f := newFakeAPI()
	defer f.srv.Close()
	f.records["z1"] = []Record{{ID: "keep", Type: "TXT", Name: "_acme-challenge.a.example.com", Content: "someone else's"}}
	c := f.client()
	remove, err := c.SetTXT(context.Background(), "_acme-challenge.a.example.com", "digest")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.records["z1"]) != 2 {
		t.Fatalf("records: %+v", f.records["z1"])
	}
	if err := remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.records["z1"]) != 1 || f.records["z1"][0].ID != "keep" {
		t.Fatalf("records after removal: %+v", f.records["z1"])
	}
}

// Account tokens (cfat_…) answer "Invalid API Token" on the user endpoint;
// found against the real API, which the fake did not model.
func TestAnAccountTokenIsVerifiedAgainstItsAccount(t *testing.T) {
	f := newFakeAPI()
	defer f.srv.Close()
	f.accountToken = true
	c := f.client()
	if st, err := c.TokenStatus(context.Background()); err != nil || st != "active" {
		t.Fatalf("account token: %q %v", st, err)
	}
	c.AccountID = ""
	if _, err := c.TokenStatus(context.Background()); err == nil {
		t.Fatal("an account token verified without its account")
	}
}
