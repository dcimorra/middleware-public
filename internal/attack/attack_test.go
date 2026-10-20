package attack

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/shots"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type fixture struct {
	g      *Guard
	h      http.Handler
	clock  *clock
	rules  *Rules
	global *Rules
}

func newFixture(t *testing.T, dir string) *fixture {
	t.Helper()
	f := &fixture{clock: &clock{t: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}, rules: &Rules{On: true, Bits: 8}}
	g, err := New(Options{
		DataDir: dir,
		Now:     f.clock.now,
		Global:  func() *Rules { return f.global },
		Panel:   func(host string) bool { return host == "panel.example" },
		Lookup: func(host string) (Site, bool) {
			return Site{Domain: host, Rules: f.rules, Title: "Shop"}, host == "shop.example" || host == "other.example"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.g = g
	f.h = g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "the site") }))
	return f
}

func (f *fixture) do(method, target, remote string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.RemoteAddr = remote
	r.Header.Set("Accept", "text/html")
	r.Header.Set("User-Agent", "Mozilla/5.0 test")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	return rec
}

var tokenRe = regexp.MustCompile(`data-challenge="([^"]+)" data-bits="(\d+)"`)

// solveIn does what the browser does, in Go.
func solveIn(t *testing.T, page string) (string, string) {
	t.Helper()
	m := tokenRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no challenge in the page:\n%s", page)
	}
	bits, _ := strconv.Atoi(m[2])
	for n := 0; ; n++ {
		if Solved(m[1], strconv.Itoa(n), bits) {
			return m[1], strconv.Itoa(n)
		}
	}
}

func (f *fixture) pass(t *testing.T, host, remote string) *http.Cookie {
	t.Helper()
	rec := f.do("GET", "http://"+host+"/shop?x=1", remote, nil)
	if rec.Code != 503 || rec.Header().Get("X-Middleware-Challenge") != "1" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src 'unsafe-inline'") {
		t.Fatalf("the first visit got %d %v", rec.Code, rec.Header())
	}
	token, nonce := solveIn(t, rec.Body.String())
	rec = f.do("POST", "http://"+host+ChallengePath, remote, url.Values{"challenge": {token}, "nonce": {nonce}, "back": {"/shop?x=1"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/shop?x=1" {
		t.Fatalf("the solution got %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	return rec.Result().Cookies()[0]
}

func TestASolvedChallengeGivesAPassBoundToItsClient(t *testing.T) {
	dir := t.TempDir()
	f := newFixture(t, dir)
	c := f.pass(t, "shop.example", "203.0.113.5:1")
	if rec := f.do("GET", "http://shop.example/", "203.0.113.5:1", nil, c); rec.Code != 200 {
		t.Fatalf("with the pass got %d", rec.Code)
	}
	// Handed to another machine, or to another site, it is worth nothing.
	if rec := f.do("GET", "http://shop.example/", "198.51.100.7:1", nil, c); rec.Code != 503 {
		t.Fatalf("another address with the pass got %d", rec.Code)
	}
	if rec := f.do("GET", "http://other.example/", "203.0.113.5:1", nil, c); rec.Code != 503 {
		t.Fatalf("another site with the pass got %d", rec.Code)
	}
	// A restart keeps every pass: the key is on disk.
	f2 := newFixture(t, dir)
	f2.clock.t = f.clock.t
	if rec := f2.do("GET", "http://shop.example/", "203.0.113.5:1", nil, c); rec.Code != 200 {
		t.Fatalf("after a restart the pass got %d", rec.Code)
	}
	// It lasts its life and no more.
	f.clock.t = f.clock.t.Add(31 * time.Minute)
	if rec := f.do("GET", "http://shop.example/", "203.0.113.5:1", nil, c); rec.Code != 503 {
		t.Fatalf("an old pass got %d", rec.Code)
	}
	if got := f.g.Counts()["shop.example"]; got.Solved != 1 || got.Served < 3 {
		t.Fatalf("counts %+v", got)
	}
}

func TestAWrongStaleOrReDifficultiedSolutionIsAnotherChallenge(t *testing.T) {
	f := newFixture(t, t.TempDir())
	rec := f.do("GET", "http://shop.example/", "203.0.113.5:1", nil)
	token, nonce := solveIn(t, rec.Body.String())
	post := func(nonce string) *httptest.ResponseRecorder {
		return f.do("POST", "http://shop.example"+ChallengePath, "203.0.113.5:1", url.Values{"challenge": {token}, "nonce": {nonce}})
	}
	if rec := post("x" + nonce); rec.Code != 503 || !strings.Contains(rec.Body.String(), "no longer valid") {
		t.Fatalf("a non-numeric nonce got %d", rec.Code)
	}
	f.rules = &Rules{On: true, Bits: 9}
	if rec := post(nonce); rec.Code != 503 {
		t.Fatalf("a challenge issued at 8 bits passed at 9: %d", rec.Code)
	}
	f.rules = &Rules{On: true, Bits: 8}
	f.clock.t = f.clock.t.Add(6 * time.Minute)
	if rec := post(nonce); rec.Code != 503 {
		t.Fatalf("a stale challenge passed: %d", rec.Code)
	}
}

func TestSkipUntilAndAPostThatCannotBeResumed(t *testing.T) {
	f := newFixture(t, t.TempDir())
	f.rules = &Rules{On: true, Bits: 8, Skip: []string{"/hooks/"}, Until: "2026-10-20T13:00:00Z"}
	if rec := f.do("POST", "http://shop.example/hooks/stripe", "203.0.113.5:1", url.Values{"a": {"b"}}); rec.Code != 200 {
		t.Fatalf("a skipped path got %d", rec.Code)
	}
	if rec := f.do("POST", "http://shop.example/Hooks/../hooks/x", "203.0.113.5:1", nil); rec.Code != 200 {
		t.Fatalf("a skipped path written differently got %d", rec.Code)
	}
	if rec := f.do("GET", "http://shop.example/hooksmith", "203.0.113.5:1", nil); rec.Code != 503 {
		t.Fatalf("skip is by segment, got %d", rec.Code)
	}
	rec := f.do("POST", "http://shop.example/cart", "203.0.113.5:1", url.Values{"item": {"1"}})
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), `name="back" value="/"`) {
		t.Fatalf("a POST must go back to the front page: %d", rec.Code)
	}
	f.clock.t = time.Date(2026, 10, 20, 13, 0, 0, 0, time.UTC)
	if rec := f.do("GET", "http://shop.example/", "203.0.113.5:1", nil); rec.Code != 200 {
		t.Fatalf("after its deadline it still charged: %d", rec.Code)
	}
}

func TestTheGlobalSwitchCoversThePanelButNotItsPrograms(t *testing.T) {
	f := newFixture(t, t.TempDir())
	f.rules = &Rules{Skip: []string{"/api"}} // the site's own switch is off
	f.global = &Rules{On: true, Bits: 8}
	if rec := f.do("GET", "http://shop.example/", "203.0.113.5:1", nil); rec.Code != 503 {
		t.Fatalf("the global switch did not reach a site: %d", rec.Code)
	}
	if rec := f.do("GET", "http://shop.example/api/x", "203.0.113.5:1", nil); rec.Code != 200 {
		t.Fatalf("the site's skip was lost under the global switch: %d", rec.Code)
	}
	if rec := f.do("POST", "http://panel.example/api/v1/auth/login", "203.0.113.5:1", nil); rec.Code != 503 {
		t.Fatalf("the panel's login was not challenged: %d", rec.Code)
	}
	r := httptest.NewRequest("GET", "http://panel.example/api/v1/sites", nil)
	r.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("a token caller was challenged: %d", rec.Code)
	}
	if rec := f.do("POST", "http://panel.example/api/v1/site/claude/ask", "203.0.113.5:1", nil); rec.Code != 200 {
		t.Fatalf("the site door was challenged: %d", rec.Code)
	}
	f.pass(t, "panel.example", "203.0.113.5:1")
	if got := f.g.Counts()[PanelName]; got.Solved != 1 {
		t.Fatalf("panel counts %+v", got)
	}
}

func TestRulesAreValidated(t *testing.T) {
	for _, r := range []*Rules{{Bits: 4}, {Bits: 30}, {Life: "10s"}, {Life: "soon"}, {Until: "tomorrow"}, {Skip: []string{"/"}}, {Skip: []string{"hooks"}}} {
		if r.Validate() == nil {
			t.Errorf("%+v validated", r)
		}
	}
	if err := (&Rules{On: true, Bits: 20, Life: "2h", Until: "2026-10-20T13:00:00Z", Skip: []string{"/hooks"}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestTheBrowserHashesWhatGoHashes runs the page's arithmetic with node.
// If it computed anything else, the browser would never find an answer
// and the site would loop on the challenge with nothing saying why.
func TestTheBrowserHashesWhatGoHashes(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	inputs := []string{"", "a", "1729.abcdef0123456789.ff|0", strings.Repeat("x", 55), strings.Repeat("y", 56), strings.Repeat("z", 64), strings.Repeat("0123456789abcdef", 9) + "|123456"}
	var js strings.Builder
	js.WriteString(hashJS)
	for _, in := range inputs {
		fmt.Fprintf(&js, "console.log(firstWord(%q));\n", in)
	}
	out, err := exec.Command(node, "-e", js.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	lines := strings.Fields(string(out))
	for i, in := range inputs {
		sum := sha256.Sum256([]byte(in))
		want := strconv.FormatUint(uint64(binary.BigEndian.Uint32(sum[:4])), 10)
		if i >= len(lines) || lines[i] != want {
			t.Fatalf("%q: node says %v, Go says %s", in, lines, want)
		}
	}
}

// TestAHeadlessChromiumGetsThroughByItself is the only proof that the page
// works: a real browser loads it, runs the script, posts the answer,
// keeps the cookie and lands on the site.
func TestAHeadlessChromiumGetsThroughByItself(t *testing.T) {
	bin := shots.Find()
	if bin == "" {
		t.Skip("no Chromium on this machine")
	}
	g, err := New(Options{
		DataDir: t.TempDir(),
		Lookup: func(host string) (Site, bool) {
			return Site{Domain: "shop.example", Rules: &Rules{On: true, Bits: 12}}, true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body><p id=in>welcome to the shop</p></body></html>")
	})))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	profile := filepath.Join(t.TempDir(), "profile")
	os.MkdirAll(profile, 0o755)
	cmd := exec.CommandContext(ctx, bin, "--headless", "--no-sandbox", "--disable-gpu", "--user-data-dir="+profile,
		"--virtual-time-budget=30000", "--dump-dom", srv.URL+"/landing")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v", bin, err)
	}
	if !strings.Contains(string(out), "welcome to the shop") {
		t.Fatalf("Chromium did not get through:\n%s", out)
	}
	if got := g.Counts()["shop.example"]; got.Solved != 1 {
		t.Fatalf("counts %+v", got)
	}
}
