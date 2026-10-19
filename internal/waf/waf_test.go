package waf

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/manifest"
)

// clock is a fake time source the tests move by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestWAF(t *testing.T, dataDir string, lookup Lookup) (*WAF, *clock, *events.Bus) {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	bus := events.New("")
	w, err := New(Options{DataDir: dataDir, Bus: bus, Lookup: lookup, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	return w, c, bus
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func get(h http.Handler, host, remote string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
	r.RemoteAddr = remote
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestARateLimitedClientGetsA429AndThenGetsBlocked(t *testing.T) {
	w, c, bus := newTestWAF(t, t.TempDir(), nil)
	p := DefaultPolicy()
	p.Rate = &manifest.Rate{Requests: 60, Window: manifest.Duration(time.Minute), Burst: 5}
	p.Trips = 3
	if err := w.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
	ch, stop := bus.Subscribe()
	defer stop()
	h := w.Wrap(okHandler())
	for i := 0; i < 5; i++ {
		if rec := get(h, "a.example", "203.0.113.5:4000", nil); rec.Code != 200 {
			t.Fatalf("request %d: got %d, want 200 within the burst", i, rec.Code)
		}
	}
	codes := []int{}
	for i := 0; i < 3; i++ {
		codes = append(codes, get(h, "a.example", "203.0.113.5:4000", nil).Code)
	}
	for _, code := range codes {
		if code != http.StatusTooManyRequests {
			t.Fatalf("got %v, want 429s once the bucket is dry", codes)
		}
	}
	if !w.IsBlocked(net.ParseIP("203.0.113.5")) {
		t.Fatal("three trips in a minute should block the address")
	}
	if rec := get(h, "a.example", "203.0.113.5:4000", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("blocked address got %d, want 403", rec.Code)
	}
	select {
	case e := <-ch:
		for e.Type != "waf.blocked" {
			e = <-ch
		}
		if e.Subject != "203.0.113.5" || e.Actor != "waf" {
			t.Fatalf("event %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no waf.blocked event")
	}
	// Another address on another site is untouched.
	if rec := get(h, "b.example", "198.51.100.9:4000", nil); rec.Code != 200 {
		t.Fatalf("unrelated client got %d", rec.Code)
	}
	// The block is ten minutes, then the bucket has refilled.
	c.advance(11 * time.Minute)
	if rec := get(h, "a.example", "203.0.113.5:4000", nil); rec.Code != 200 {
		t.Fatalf("after the block expired got %d, want 200", rec.Code)
	}
	if hits := w.Recent(); len(hits) != 0 {
		t.Fatalf("recent should be empty after a quiet minute, got %+v", hits)
	}
}

func TestEachAutomaticBlockLastsLongerThanThePrevious(t *testing.T) {
	w, c, _ := newTestWAF(t, t.TempDir(), nil)
	p := DefaultPolicy()
	p.Rate = &manifest.Rate{Requests: 1, Window: manifest.Duration(time.Hour), Burst: 1}
	p.Trips = 1
	if err := w.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
	h := w.Wrap(okHandler())
	ip := net.ParseIP("203.0.113.7")
	var durations []time.Duration
	for round := 0; round < 3; round++ {
		get(h, "a.example", "203.0.113.7:1", nil)
		get(h, "a.example", "203.0.113.7:1", nil)
		var until time.Time
		for _, e := range w.Blocked() {
			if e.IP == ip.String() {
				until = e.Until
			}
		}
		if until.IsZero() {
			t.Fatalf("round %d: not blocked", round)
		}
		durations = append(durations, until.Sub(c.now()))
		c.advance(until.Sub(c.now()) + time.Hour)
	}
	if !(durations[0] < durations[1] && durations[1] < durations[2]) {
		t.Fatalf("durations should rise: %v", durations)
	}
}

func TestAnAllowedNetworkBypassesTheRateLimit(t *testing.T) {
	w, _, _ := newTestWAF(t, t.TempDir(), nil)
	p := DefaultPolicy()
	p.Rate = &manifest.Rate{Requests: 1, Window: manifest.Duration(time.Hour), Burst: 1}
	p.Allow = []string{"10.0.0.0/8"}
	if err := w.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
	h := w.Wrap(okHandler())
	for i := 0; i < 20; i++ {
		if rec := get(h, "a.example", "10.1.2.3:5", nil); rec.Code != 200 {
			t.Fatalf("allowed client got %d on request %d", rec.Code, i)
		}
	}
	if err := w.Block(net.ParseIP("10.1.2.3"), time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	if rec := get(h, "a.example", "10.1.2.3:5", nil); rec.Code != 200 {
		t.Fatalf("the allow list should even beat the block list, got %d", rec.Code)
	}
}

func TestTheSiteManifestTightensTheGlobalRate(t *testing.T) {
	site := Site{Domain: "tight.example", WAF: &manifest.WAF{Rate: &manifest.Rate{Requests: 2, Window: manifest.Duration(time.Minute)}}}
	lookup := func(host string) (Site, bool) {
		if host == site.Domain {
			return site, true
		}
		return Site{}, false
	}
	w, _, _ := newTestWAF(t, t.TempDir(), lookup)
	h := w.Wrap(okHandler())
	get(h, "tight.example", "203.0.113.1:1", nil)
	get(h, "tight.example", "203.0.113.1:1", nil)
	if rec := get(h, "tight.example", "203.0.113.1:1", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third request to a site with rate 2/min got %d", rec.Code)
	}
	if rec := get(h, "loose.example", "203.0.113.1:1", nil); rec.Code != 200 {
		t.Fatalf("the same client on a site with the default policy got %d", rec.Code)
	}
}

func TestTheChallengeCookieRoundTrips(t *testing.T) {
	w, c, _ := newTestWAF(t, t.TempDir(), nil)
	if err := w.SetUnderAttack("", true); err != nil {
		t.Fatal(err)
	}
	h := w.Wrap(okHandler())
	first := get(h, "a.example", "203.0.113.9:1", nil)
	if first.Code != http.StatusServiceUnavailable {
		t.Fatalf("first visit got %d, want the challenge", first.Code)
	}
	if first.Header().Get("Refresh") != "2" {
		t.Fatal("the challenge must tell the browser to come back")
	}
	cookies := first.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != clearanceCookie {
		t.Fatalf("cookies: %+v", cookies)
	}
	cookie := cookies[0].Name + "=" + cookies[0].Value
	if rec := get(h, "a.example", "203.0.113.9:1", map[string]string{"Cookie": cookie}); rec.Code != 200 {
		t.Fatalf("with the cookie got %d, want 200", rec.Code)
	}
	// The cookie is bound to the address: another client cannot reuse it.
	if rec := get(h, "a.example", "203.0.113.10:1", map[string]string{"Cookie": cookie}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("another address with a stolen cookie got %d, want the challenge", rec.Code)
	}
	// A bot that never keeps cookies never gets in.
	if rec := get(h, "a.example", "203.0.113.9:1", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("without the cookie got %d", rec.Code)
	}
	// And a clearance expires.
	c.advance(2 * time.Hour)
	if rec := get(h, "a.example", "203.0.113.9:1", map[string]string{"Cookie": cookie}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expired cookie got %d", rec.Code)
	}
	// An allowed crawler is let through without one.
	p := w.Policy()
	p.AllowAgents = []string{"Googlebot"}
	if err := w.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
	if rec := get(h, "a.example", "203.0.113.11:1", map[string]string{"User-Agent": "Mozilla/5.0 (compatible; Googlebot/2.1)"}); rec.Code != 200 {
		t.Fatalf("allowed crawler got %d", rec.Code)
	}
}

func TestUnderAttackPerSiteIsWrittenToTheManifest(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sites", "s.example")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Save(dir, &manifest.Manifest{Version: manifest.Version}); err != nil {
		t.Fatal(err)
	}
	lookup := func(host string) (Site, bool) {
		if host == "s.example" {
			return Site{Domain: host, Dir: dir}, true
		}
		return Site{}, false
	}
	w, _, _ := newTestWAF(t, filepath.Join(root, "data"), lookup)
	if err := w.SetUnderAttack("s.example", true); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.WAF == nil || !m.WAF.UnderAttack {
		t.Fatal("under_attack was not written to site.json")
	}
	if err := w.SetUnderAttack("nope.example", true); err == nil {
		t.Fatal("an unknown site must be an error")
	}
}

func TestABodyOverTheLimitIsRefused(t *testing.T) {
	w, _, _ := newTestWAF(t, t.TempDir(), nil)
	p := DefaultPolicy()
	p.MaxBody = "1k"
	if err := w.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
	var readErr error
	h := w.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		_, readErr = r.Body.Read(buf)
		rw.WriteHeader(200)
	}))
	// Declared length: refused before the handler runs.
	r := httptest.NewRequest(http.MethodPost, "http://a.example/", strings.NewReader(strings.Repeat("x", 2048)))
	r.RemoteAddr = "203.0.113.2:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413", rec.Code)
	}
	// Chunked (unknown length): the reader stops the handler.
	r = httptest.NewRequest(http.MethodPost, "http://a.example/", strings.NewReader(strings.Repeat("x", 2048)))
	r.ContentLength = -1
	r.RemoteAddr = "203.0.113.2:1"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if readErr == nil {
		t.Fatal("reading past the limit should fail")
	}
	// Small bodies pass.
	r = httptest.NewRequest(http.MethodPost, "http://a.example/", strings.NewReader("hello"))
	r.RemoteAddr = "203.0.113.2:1"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("small body got %d", rec.Code)
	}
}

func TestTheBlockListSurvivesAReloadFromDisk(t *testing.T) {
	dir := t.TempDir()
	w, c, _ := newTestWAF(t, dir, nil)
	if err := w.Block(net.ParseIP("203.0.113.3"), time.Hour, "manual"); err != nil {
		t.Fatal(err)
	}
	if err := w.Block(net.ParseIP("2001:db8::1"), time.Minute, "manual"); err != nil {
		t.Fatal(err)
	}
	// A second WAF over the same data dir (a restart, or the CLI).
	w2, err := New(Options{DataDir: dir, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(w2.Blocked()); got != 2 {
		t.Fatalf("reloaded %d entries, want 2", got)
	}
	if !w2.IsBlocked(net.ParseIP("203.0.113.3")) {
		t.Fatal("reloaded list does not block the address")
	}
	// Unblock from the second instance; the first picks it up on Reload.
	if err := w2.Unblock(net.ParseIP("203.0.113.3")); err != nil {
		t.Fatal(err)
	}
	// Force a different mtime: the reload is mtime-driven.
	time.Sleep(20 * time.Millisecond)
	if err := w2.Block(net.ParseIP("203.0.113.4"), time.Hour, "cli"); err != nil {
		t.Fatal(err)
	}
	if err := w.Reload(); err != nil {
		t.Fatal(err)
	}
	if w.IsBlocked(net.ParseIP("203.0.113.3")) || !w.IsBlocked(net.ParseIP("203.0.113.4")) {
		t.Fatalf("reload did not pick up the other process's changes: %+v", w.Blocked())
	}
	// Expired entries are gone after the grace period.
	c.advance(26 * time.Hour)
	if got := len(w.Blocked()); got != 0 {
		t.Fatalf("expired entries still listed: %d", got)
	}
}

func TestABrokenPolicyFileIsLoudButLeavesTheWAFRunning(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "waf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "waf", "policy.json"), []byte(`{"rate": {"requests": 0, "window": "1m"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := New(Options{DataDir: dir})
	var pe *PolicyError
	if !errors.As(err, &pe) || w == nil {
		t.Fatalf("got %v / %v, want a usable WAF and a PolicyError", w, err)
	}
	if w.Policy().Rate == nil || w.Policy().Rate.Requests != DefaultPolicy().Rate.Requests {
		t.Fatalf("not on defaults: %+v", w.Policy())
	}
}

func TestLoopbackIsNeverBlocked(t *testing.T) {
	w, _, _ := newTestWAF(t, t.TempDir(), nil)
	if err := w.Block(net.ParseIP("127.0.0.1"), time.Hour, "oops"); err == nil {
		t.Fatal("blocking loopback would block the tunnel and the panel")
	}
}

func TestDeniedAgentsAndCountriesGet403(t *testing.T) {
	w, _, _ := newTestWAF(t, t.TempDir(), nil)
	w.opts.Country = func(ip net.IP) string {
		if ip.Equal(net.ParseIP("203.0.113.50")) {
			return "xx"
		}
		return ""
	}
	p := DefaultPolicy()
	p.DenyAgents = []string{"EvilBot"}
	p.DenyCountries = []string{"XX"}
	if err := w.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
	h := w.Wrap(okHandler())
	if rec := get(h, "a.example", "203.0.113.51:1", map[string]string{"User-Agent": "evilbot/1.0"}); rec.Code != http.StatusForbidden {
		t.Fatalf("denied agent got %d", rec.Code)
	}
	if rec := get(h, "a.example", "203.0.113.50:1", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("denied country got %d", rec.Code)
	}
	if rec := get(h, "a.example", "203.0.113.51:1", nil); rec.Code != 200 {
		t.Fatalf("plain client got %d", rec.Code)
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

func TestPolicyValidationRejectsWhatCannotBeApplied(t *testing.T) {
	w, _, _ := newTestWAF(t, t.TempDir(), nil)
	bad := []Policy{
		{Rate: &manifest.Rate{Requests: 0, Window: manifest.Duration(time.Minute)}},
		{MaxBody: "lots"},
		{Allow: []string{"not-an-address"}},
		{DenyCountries: []string{"ESP"}},
		{Trips: -1},
	}
	for i, p := range bad {
		if err := w.SetPolicy(p); err == nil {
			t.Errorf("policy %d accepted: %+v", i, p)
		}
	}
	if _, err := ParseSize("20m"); err != nil {
		t.Fatal(err)
	}
	if n, _ := ParseSize("512k"); n != 512<<10 {
		t.Fatalf("512k = %d", n)
	}
}
