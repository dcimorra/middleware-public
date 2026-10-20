package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain lowers the PBKDF2 cost: the hash format is checked at full
// strength in TestAHashedPasswordVerifiesAndAWrongOneDoesNot and the rest
// of the suite only needs a hash that verifies.
func TestMain(m *testing.M) {
	iterations = 1000
	os.Exit(m.Run())
}

func newStore(t *testing.T) *Store {
	t.Helper()
	return New(t.TempDir(), nil)
}

func TestAHashedPasswordVerifiesAndAWrongOneDoesNot(t *testing.T) {
	iterations = Iterations
	defer func() { iterations = 1000 }()
	phc, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$pbkdf2-sha256$i=600000$") {
		t.Fatalf("not a PHC string: %s", phc)
	}
	if !VerifyPassword(phc, "correct horse battery") {
		t.Fatal("the right password does not verify")
	}
	if VerifyPassword(phc, "correct horse batteri") {
		t.Fatal("a wrong password verifies")
	}
	if VerifyPassword("garbage", "anything") {
		t.Fatal("a malformed hash verifies")
	}
}

func TestAShortPasswordIsRefused(t *testing.T) {
	if _, err := HashPassword("short"); err != ErrWeakPassword {
		t.Fatalf("got %v", err)
	}
}

func TestTheSetupCodeWorksOnceAndIsGoneAfterwards(t *testing.T) {
	s := newStore(t)
	code, err := s.CreateSetupCode()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Setup("not-the-code", "password-1"); err != ErrBadSetupCode {
		t.Fatalf("wrong code accepted: %v", err)
	}
	if err := s.Setup(code, "password-1"); err != nil {
		t.Fatal(err)
	}
	if s.HasSetupCode() {
		t.Fatal("the code file survived its use")
	}
	if err := s.Setup(code, "password-2"); err != ErrBadSetupCode {
		t.Fatalf("the code worked twice: %v", err)
	}
	if !s.HasOperator() {
		t.Fatal("no operator after setup")
	}
	st, _ := os.Stat(filepath.Join(s.dir, "password"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("password file mode %o", st.Mode().Perm())
	}
}

func TestLoginGivesASessionThatSurvivesAReopen(t *testing.T) {
	s := newStore(t)
	if _, err := s.Login("password-1", "10.0.0.1"); err != ErrNoOperator {
		t.Fatalf("login without an operator: %v", err)
	}
	if err := s.SetPassword("password-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login("wrong-password", "10.0.0.1"); err != ErrBadCredentials {
		t.Fatalf("wrong password: %v", err)
	}
	id, err := s.Login("password-1", "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !s.ValidSession(id) {
		t.Fatal("fresh session invalid")
	}
	again := New(filepath.Dir(filepath.Dir(s.dir)), nil)
	if !again.ValidSession(id) {
		t.Fatal("the session did not survive a reopen")
	}
	if err := again.DeleteSession(id); err != nil {
		t.Fatal(err)
	}
	if again.ValidSession(id) || New(filepath.Dir(filepath.Dir(s.dir)), nil).ValidSession(id) {
		t.Fatal("deleted session still valid")
	}
}

func TestASessionExpiresAfterThirtyDays(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	id, _ := s.NewSession()
	now = now.Add(SessionTTL + time.Second)
	if s.ValidSession(id) {
		t.Fatal("expired session still valid")
	}
}

func TestChangingThePasswordDropsEverySession(t *testing.T) {
	s := newStore(t)
	s.SetPassword("password-1")
	id, _ := s.Login("password-1", "10.0.0.1")
	s.SetPassword("password-2")
	if s.ValidSession(id) {
		t.Fatal("session survived a password change")
	}
}

func TestFiveFailuresInAMinuteLockTheAddressAndTheLockDoubles(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	s.limiter.Now = s.now
	s.SetPassword("password-1")
	for i := 0; i < 5; i++ {
		if _, err := s.Login("wrong", "10.0.0.1"); err != ErrBadCredentials {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := s.Login("password-1", "10.0.0.1"); err != ErrRateLimited {
		t.Fatalf("sixth attempt not limited: %v", err)
	}
	if _, err := s.Login("password-1", "10.0.0.2"); err != nil {
		t.Fatalf("another address was locked out too: %v", err)
	}
	now = now.Add(61 * time.Second)
	for i := 0; i < 5; i++ {
		s.Login("wrong", "10.0.0.1")
	}
	now = now.Add(90 * time.Second)
	if _, err := s.Login("password-1", "10.0.0.1"); err != ErrRateLimited {
		t.Fatalf("the second lock did not double: %v", err)
	}
	now = now.Add(60 * time.Second)
	if _, err := s.Login("password-1", "10.0.0.1"); err != nil {
		t.Fatalf("still locked after the doubled window: %v", err)
	}
}

func TestATokenIsShownOnceAndOnlyItsHashIsKept(t *testing.T) {
	s := newStore(t)
	pub, secret, err := s.CreateToken("deploy script")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "mw_") {
		t.Fatalf("secret %q", secret)
	}
	raw, _ := os.ReadFile(filepath.Join(s.dir, "tokens.json"))
	if strings.Contains(string(raw), secret) {
		t.Fatal("the secret is on disk")
	}
	got, ok := s.LookupToken(secret)
	if !ok || got.ID != pub.ID || got.Name != "deploy script" {
		t.Fatalf("lookup: %+v %v", got, ok)
	}
	if _, ok := s.LookupToken("mw_nope"); ok {
		t.Fatal("unknown token resolved")
	}
	list, _ := s.Tokens()
	if len(list) != 1 || list[0].LastUsed.IsZero() {
		t.Fatalf("list %+v", list)
	}
	if err := s.DeleteToken(pub.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LookupToken(secret); ok {
		t.Fatal("deleted token still resolves")
	}
	if err := s.DeleteToken(pub.ID); err != ErrNotFound {
		t.Fatalf("deleting twice: %v", err)
	}
	if _, _, err := s.CreateToken("  "); err != ErrTokenNameNeeded {
		t.Fatalf("unnamed token: %v", err)
	}
}

func TestMiddlewareResolvesSocketTokenAndCookieInThatOrder(t *testing.T) {
	s := newStore(t)
	s.SetPassword("password-1")
	sid, _ := s.Login("password-1", "10.0.0.1")
	_, secret, _ := s.CreateToken("t")

	var seen Caller
	h := s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = FromContext(r.Context()) }))

	r := httptest.NewRequest("GET", "/", nil)
	h.ServeHTTP(httptest.NewRecorder(), r)
	if seen.Authenticated() {
		t.Fatal("anonymous request authenticated")
	}

	r = httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: sid})
	h.ServeHTTP(httptest.NewRecorder(), r)
	if seen.Via != ViaCookie {
		t.Fatalf("cookie: %+v", seen)
	}

	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	h.ServeHTTP(httptest.NewRecorder(), r)
	if seen.Via != ViaToken || seen.Token != "t" {
		t.Fatalf("token: %+v", seen)
	}

	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer mw_wrong")
	r.AddCookie(&http.Cookie{Name: CookieName, Value: sid})
	h.ServeHTTP(httptest.NewRecorder(), r)
	if seen.Authenticated() {
		t.Fatal("a wrong bearer must not fall back to the cookie")
	}

	r = httptest.NewRequest("GET", "/", nil).WithContext(WithSocket(httptest.NewRequest("GET", "/", nil).Context()))
	h.ServeHTTP(httptest.NewRecorder(), r)
	if seen.Via != ViaSocket {
		t.Fatalf("socket: %+v", seen)
	}
}

func TestTheCookieIsSecureOnlyBehindHTTPS(t *testing.T) {
	w := httptest.NewRecorder()
	SetCookie(w, httptest.NewRequest("GET", "http://localhost/", nil), "x")
	c := w.Result().Cookies()[0]
	if c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge != int(SessionTTL.Seconds()) {
		t.Fatalf("plain http cookie: %+v", c)
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://panel.test/", nil)
	r.RemoteAddr = "127.0.0.1:5555" // the tunnel
	r.Header.Set("X-Forwarded-Proto", "https")
	SetCookie(w, r, "x")
	if !w.Result().Cookies()[0].Secure {
		t.Fatal("not Secure behind an https proxy")
	}
}

func TestClientIPPrefersTheEdgeHeader(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	if ClientIP(r) != "127.0.0.1" {
		t.Fatal(ClientIP(r))
	}
	// The last hop is the one the trusted proxy appended; the ones before
	// it are whatever the client wrote.
	r.Header.Set("X-Forwarded-For", "10.0.0.1, 203.0.113.9")
	if ClientIP(r) != "203.0.113.9" {
		t.Fatal(ClientIP(r))
	}
	r.Header.Set("CF-Connecting-IP", "198.51.100.2")
	if ClientIP(r) != "198.51.100.2" {
		t.Fatal(ClientIP(r))
	}
	// A client at the 80/443 door rotating the header would get a fresh
	// brake on every guess.
	r.RemoteAddr = "203.0.113.7:5555"
	if ClientIP(r) != "203.0.113.7" {
		t.Fatalf("a direct client named its own address: %s", ClientIP(r))
	}
}

func TestATokenIDNeverLooksLikeAFlag(t *testing.T) {
	s := newStore(t)
	s.SetPassword("password-1")
	for i := 0; i < 300; i++ {
		p, _, err := s.CreateToken("t")
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(p.ID, "-") || strings.Trim(p.ID, "0123456789abcdef") != "" {
			t.Fatalf("token id %q", p.ID)
		}
	}
}
