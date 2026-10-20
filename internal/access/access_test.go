package access

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/manifest"
)

func TestAPasswordHashRoundTripsAndRejectsTheWrongOne(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$pbkdf2-sha256$50000$") || !IsHash(h) {
		t.Fatalf("unexpected format %q", h)
	}
	if !CheckPassword(h, "correct horse") {
		t.Fatal("the right password does not check")
	}
	if CheckPassword(h, "battery staple") || CheckPassword(h, "") || CheckPassword("", "x") {
		t.Fatal("a wrong or empty password checked")
	}
	if CheckPassword("$pbkdf2-sha256$notanumber$abc$def", "x") || CheckPassword("plain", "plain") {
		t.Fatal("a malformed hash must never match")
	}
	h2, _ := HashPassword("correct horse")
	if h2 == h {
		t.Fatal("two hashes of the same password share a salt")
	}
	if _, err := HashPassword(""); err == nil {
		t.Fatal("an empty password must be refused")
	}
}

func siteWith(ac *manifest.Access) Lookup {
	return func(host string) (Site, bool) {
		if host == "p.example" {
			return Site{Domain: host, Access: ac}, true
		}
		return Site{}, false
	}
}

func newAccess(t *testing.T, lookup Lookup) *Access {
	t.Helper()
	a, err := New(Options{DataDir: t.TempDir(), Lookup: lookup})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
}

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func req(method, path, remote string) *http.Request {
	r := httptest.NewRequest(method, "http://p.example"+path, nil)
	r.RemoteAddr = remote
	return r
}

func TestAnAddressOutsideTheAllowListGets403(t *testing.T) {
	a := newAccess(t, siteWith(&manifest.Access{Allow: []string{"10.0.0.0/8", "192.0.2.7"}}))
	h := a.Wrap(okHandler())
	if rec := do(h, req("GET", "/", "203.0.113.1:1")); rec.Code != http.StatusForbidden {
		t.Fatalf("outside got %d", rec.Code)
	}
	if rec := do(h, req("GET", "/", "10.9.8.7:1")); rec.Code != 200 {
		t.Fatalf("inside got %d", rec.Code)
	}
	if rec := do(h, req("GET", "/", "192.0.2.7:1")); rec.Code != 200 {
		t.Fatalf("single address got %d", rec.Code)
	}
	// A site without an access section is open.
	r := httptest.NewRequest("GET", "http://other.example/", nil)
	r.RemoteAddr = "203.0.113.1:1"
	if rec := do(h, r); rec.Code != 200 {
		t.Fatalf("unrelated site got %d", rec.Code)
	}
}

func TestTheLongestRoutePrefixWins(t *testing.T) {
	ac := &manifest.Access{
		Allow: []string{"10.0.0.0/8"},
		Routes: map[string]*manifest.RouteAccess{
			"/public":       {Allow: []string{"0.0.0.0/0"}},
			"/public/inner": {Allow: []string{"192.0.2.0/24"}},
		},
	}
	a := newAccess(t, siteWith(ac))
	h := a.Wrap(okHandler())
	cases := []struct {
		path, remote string
		want         int
	}{
		{"/", "203.0.113.1:1", 403},
		{"/public", "203.0.113.1:1", 200},
		{"/public/x", "203.0.113.1:1", 200},
		{"/publicity", "203.0.113.1:1", 403},
		{"/public/inner/x", "203.0.113.1:1", 403},
		{"/public/inner/x", "192.0.2.9:1", 200},
	}
	for _, tc := range cases {
		if rec := do(h, req("GET", tc.path, tc.remote)); rec.Code != tc.want {
			t.Errorf("%s from %s: got %d, want %d", tc.path, tc.remote, rec.Code, tc.want)
		}
	}
}

func TestTheLoginFormSetsASessionThatLetsTheVisitorIn(t *testing.T) {
	hash, _ := HashPassword("open sesame")
	a := newAccess(t, siteWith(&manifest.Access{Password: hash}))
	h := a.Wrap(okHandler())
	r := req("GET", "/secret?x=1", "203.0.113.1:1")
	r.Header.Set("Accept", "text/html")
	rec := do(h, r)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `name="password"`) {
		t.Fatalf("browser got %d, want the form; body %q", rec.Code, rec.Body.String()[:80])
	}
	if !strings.Contains(rec.Body.String(), `value="/secret?x=1"`) {
		t.Fatal("the form must remember where the visitor was going")
	}
	// Wrong password: the form again, no cookie.
	form := url.Values{"password": {"nope"}, "next": {"/secret?x=1"}}
	r = httptest.NewRequest("POST", "http://p.example"+LoginPath, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Accept", "text/html")
	rec = do(h, r)
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("wrong password got %d with %d cookies", rec.Code, len(rec.Result().Cookies()))
	}
	// Right password: cookie and redirect to next.
	form.Set("password", "open sesame")
	r = httptest.NewRequest("POST", "http://p.example"+LoginPath, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = do(h, r)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/secret?x=1" {
		t.Fatalf("login got %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookie {
		t.Fatalf("cookies %+v", cookies)
	}
	r = req("GET", "/secret", "203.0.113.1:1")
	r.AddCookie(cookies[0])
	if rec := do(h, r); rec.Code != 200 {
		t.Fatalf("with the session got %d", rec.Code)
	}
	// Changing the password ends the session.
	newHash, _ := HashPassword("something else")
	a2 := &Access{opts: a.opts, now: a.now, key: a.key, verified: map[[32]byte]struct{}{}}
	a2.opts.Lookup = siteWith(&manifest.Access{Password: newHash})
	r = req("GET", "/secret", "203.0.113.1:1")
	r.AddCookie(cookies[0])
	if rec := do(a2.Wrap(okHandler()), r); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old session after a password change got %d", rec.Code)
	}
	// An open redirect is not a redirect.
	form.Set("next", "//evil.example/x")
	r = httptest.NewRequest("POST", "http://p.example"+LoginPath, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := do(h, r); rec.Header().Get("Location") != "/" {
		t.Fatalf("next=//evil redirected to %q", rec.Header().Get("Location"))
	}
}

func TestHTTPBasicAuthWorksForTools(t *testing.T) {
	hash, _ := HashPassword("tool pass")
	a := newAccess(t, siteWith(&manifest.Access{Password: hash}))
	h := a.Wrap(okHandler())
	rec := do(h, req("GET", "/api", "203.0.113.1:1"))
	if rec.Code != http.StatusUnauthorized || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("a non-browser got %d without a Basic challenge", rec.Code)
	}
	r := req("GET", "/api", "203.0.113.1:1")
	r.SetBasicAuth("anyone", "tool pass")
	if rec := do(h, r); rec.Code != 200 {
		t.Fatalf("basic auth got %d", rec.Code)
	}
	// The second check comes from the cache, which must still say yes,
	// and a wrong password must still say no.
	if rec := do(h, r); rec.Code != 200 {
		t.Fatalf("cached basic auth got %d", rec.Code)
	}
	r.SetBasicAuth("anyone", "wrong")
	if rec := do(h, r); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong basic auth got %d", rec.Code)
	}
}

func TestARoutePasswordLeavesTheRestOfTheSiteOpen(t *testing.T) {
	hash, _ := HashPassword("admin pass")
	ac := &manifest.Access{Password: hash, Routes: map[string]*manifest.RouteAccess{"/admin": {Password: true}}}
	a := newAccess(t, siteWith(ac))
	h := a.Wrap(okHandler())
	if rec := do(h, req("GET", "/", "203.0.113.1:1")); rec.Code != 200 {
		t.Fatalf("the open part got %d", rec.Code)
	}
	if rec := do(h, req("GET", "/admin/users", "203.0.113.1:1")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the protected part got %d", rec.Code)
	}
}

func TestDeniedCountriesGet403(t *testing.T) {
	a := newAccess(t, siteWith(&manifest.Access{DenyCountries: []string{"xx"}}))
	a.opts.Country = func(_ *http.Request, ip net.IP) string {
		if ip.String() == "203.0.113.1" {
			return "XX"
		}
		return "YY"
	}
	h := a.Wrap(okHandler())
	if rec := do(h, req("GET", "/", "203.0.113.1:1")); rec.Code != http.StatusForbidden {
		t.Fatalf("denied country got %d", rec.Code)
	}
	if rec := do(h, req("GET", "/", "203.0.113.2:1")); rec.Code != 200 {
		t.Fatalf("other country got %d", rec.Code)
	}
}

func TestTheSessionKeySurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	a1, err := New(Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	a2, err := New(Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if string(a1.key) != string(a2.key) {
		t.Fatal("a new key per boot would log everyone out on restart")
	}
	st, err := os.Stat(filepath.Join(dir, "access", "key"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %o, want 0600", st.Mode().Perm())
	}
}

func TestAZoneCannotBeSkippedWithDotsSlashesOrCase(t *testing.T) {
	hash, _ := HashPassword("pw")
	a := newAccess(t, siteWith(&manifest.Access{Password: hash, Routes: map[string]*manifest.RouteAccess{"/admin": {Password: true}}}))
	h := a.Wrap(okHandler())
	if rec := do(h, req("GET", "/", "203.0.113.1:1")); rec.Code != 200 {
		t.Fatalf("the open part got %d", rec.Code)
	}
	for _, p := range []string{"/admin", "/x/../admin", "//admin/users", "/ADMIN", "/Admin/", "/admin;x=1/users", "/./admin"} {
		r := req("GET", "/", "203.0.113.1:1")
		r.URL.Path = p
		if rec := do(h, r); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s got %d", p, rec.Code)
		}
	}
}

func TestWithoutItsKeyTheDoorStaysShut(t *testing.T) {
	hash, _ := HashPassword("pw")
	sites := map[string]*manifest.Access{"p.example": {Password: hash}, "o.example": nil}
	lookup := func(host string) (Site, bool) { ac, ok := sites[host]; return Site{Domain: host, Access: ac}, ok }
	h := Closed(lookup)(okHandler())
	r := httptest.NewRequest("GET", "http://p.example/", nil)
	if rec := do(h, r); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a protected site was served open: %d", rec.Code)
	}
	r = httptest.NewRequest("GET", "http://o.example/", nil)
	if rec := do(h, r); rec.Code != 200 {
		t.Fatalf("an open site was closed: %d", rec.Code)
	}
}
