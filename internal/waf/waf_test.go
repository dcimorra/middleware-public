package waf

import (
	"errors"
	"github.com/dcimorra/middleware-public/internal/attack"
	"io"
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

func TestTheGlobalSwitchMovesTheOldBooleanToTheAttackBlock(t *testing.T) {
	w, _, _ := newTestWAF(t, t.TempDir(), nil)
	p := w.Policy()
	p.UnderAttack = true
	if err := w.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
	if g := w.GlobalAttack(); g == nil || !g.On {
		t.Fatalf("the old boolean is not read as on: %+v", g)
	}
	if err := w.SetAttack("", func(r *attack.Rules) { r.Bits = 12 }); err != nil {
		t.Fatal(err)
	}
	p = w.Policy()
	if p.UnderAttack || p.Attack == nil || !p.Attack.On || p.Attack.Bits != 12 {
		t.Fatalf("policy after the switch: %+v %+v", p, p.Attack)
	}
	if err := w.SetAttack("", func(r *attack.Rules) { r.Bits = 40 }); err == nil {
		t.Fatal("40 bits were accepted")
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
	if err := w.SetAttack("s.example", func(r *attack.Rules) { r.On, r.Skip = true, []string{"/hooks"} }); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Attack == nil || !m.Attack.On || m.Attack.Skip[0] != "/hooks" || m.WAF != nil {
		t.Fatalf("attack was not written to site.json: %+v %+v", m.Attack, m.WAF)
	}
	if err := w.SetAttack("nope.example", func(r *attack.Rules) { r.On = true }); err == nil {
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
	w.opts.Country = func(_ *http.Request, ip net.IP) string {
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

// A site's bundle moving into this house is gigabytes on purpose and
// authorized by the API: max_body does not cut it at the panel. A site's
// own path that looks the same keeps its limit, and so does anything else.
func TestAMovesUploadIsNotCutByMaxBodyAtThePanel(t *testing.T) {
	w, _, _ := newTestWAF(t, t.TempDir(), func(host string) (Site, bool) { return Site{}, host == "shop.example" })
	p := DefaultPolicy()
	p.MaxBody = "1k"
	if err := w.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
	h := w.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			rw.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		rw.WriteHeader(200)
	}))
	send := func(method, url string) int {
		r := httptest.NewRequest(method, url, strings.NewReader(strings.Repeat("x", 4096)))
		r.ContentLength = -1 // streamed, as a move sends it
		r.RemoteAddr = "203.0.113.2:1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	for url, want := range map[string]int{
		"http://panel.example/api/v1/relocate/napnap.es":       200,
		"http://panel.example/api/v1/relocate/napnap.es/delta": 200,
		"http://panel.example/api/v1/relocate/napnap.es/other": 413,
		"http://panel.example/api/v1/sites":                    413,
		"http://shop.example/api/v1/relocate/napnap.es":        413,
	} {
		if got := send(http.MethodPut, url); got != want {
			t.Errorf("PUT %s: %d, want %d", url, got, want)
		}
	}
	if got := send(http.MethodPost, "http://panel.example/api/v1/relocate/napnap.es"); got != 413 {
		t.Errorf("POST to the upload's path: %d, want 413", got)
	}
}

// The operator's own session on the panel is not rate-limited nor tripped
// into a block, from whatever address: the same burst without it is.
func TestTheOperatorsOwnPanelTrafficIsNotRateLimited(t *testing.T) {
	w, _, _ := newTestWAF(t, t.TempDir(), nil)
	p := DefaultPolicy()
	p.Rate = &manifest.Rate{Requests: 5, Window: manifest.Duration(time.Minute), Burst: 5}
	p.Trips = 3
	if err := w.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
	w.SetOperator(func(r *http.Request) bool { return r.Header.Get("Cookie") == "mw_session=good" })
	h := w.Wrap(okHandler())
	send := func(remote, cookie string) int {
		r := httptest.NewRequest(http.MethodGet, "http://panel.example/api/v1/agents/waiting", nil)
		r.RemoteAddr = remote
		if cookie != "" {
			r.Header.Set("Cookie", cookie)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	for i := 0; i < 60; i++ {
		if c := send("203.0.113.7:1", "mw_session=good"); c != 200 {
			t.Fatalf("the operator's request %d: %d", i, c)
		}
	}
	if w.IsBlocked(net.ParseIP("203.0.113.7")) {
		t.Fatal("the operator's address was blocked")
	}
	limited := false
	for i := 0; i < 60; i++ {
		if c := send("203.0.113.8:1", "mw_session=stolen-or-none"); c == http.StatusTooManyRequests || c == http.StatusForbidden {
			limited = true
		}
	}
	if !limited || !w.IsBlocked(net.ParseIP("203.0.113.8")) {
		t.Fatal("the same burst without the session was not limited and blocked")
	}
}

// The operator's address, trusted at sign-in, gets the 429s of a site it
// reloads too fast and never the block, which goes to the firewall and
// closes every site and the panel at once.
func TestTheOperatorsAddressIsNeverBlockedForGoingTooFast(t *testing.T) {
	w, _, _ := newTestWAF(t, t.TempDir(), nil)
	p := DefaultPolicy()
	p.Rate = &manifest.Rate{Requests: 60, Window: manifest.Duration(time.Minute), Burst: 5}
	p.Trips = 3
	if err := w.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
	w.Trust(net.ParseIP("46.136.139.2"), 7*24*time.Hour)
	h := w.Wrap(okHandler())
	limited := false
	for i := 0; i < 40; i++ {
		if get(h, "luma.example", "46.136.139.2:5000", nil).Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("the burst was not limited at all")
	}
	if w.IsBlocked(net.ParseIP("46.136.139.2")) {
		t.Fatal("the operator's address was blocked")
	}
}

