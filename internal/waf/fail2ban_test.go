package waf

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func at(h http.Handler, target, remote string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "http://shop.test"+target, nil)
	r.RemoteAddr = remote
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func notFound() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
}

// Log4Shell in a header: refused, and the address is cut there and then,
// in the kernel too.
func TestAnExploitIsRefusedAndBansAtOnce(t *testing.T) {
	w, _, _ := newTestWAF(t, t.TempDir(), nil)
	var kernel []string
	w.OnBlock = func(ip net.IP, d time.Duration) { kernel = append(kernel, ip.String()+" "+d.String()) }
	h := w.Wrap(okHandler())
	rec := at(h, "/", "198.51.100.9:4000", map[string]string{"User-Agent": "${jndi:ldap://x/a}"})
	if rec.Code != http.StatusForbidden || !w.IsBlocked(net.ParseIP("198.51.100.9")) {
		t.Fatalf("%d blocked=%v", rec.Code, w.IsBlocked(net.ParseIP("198.51.100.9")))
	}
	if len(kernel) != 1 || kernel[0] != "198.51.100.9 1h0m0s" {
		t.Fatalf("kernel %v", kernel)
	}
	if rec := at(h, "/", "198.51.100.9:4000", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("still served: %d", rec.Code)
	}
	for _, target := range []string{"/..%2f..%2fetc/passwd", "/index.php?page=php://filter/resource=x", "/vendor/phpunit/phpunit/src/Util/PHP/eval-stdin.php"} {
		w2, _, _ := newTestWAF(t, t.TempDir(), nil)
		if rec := at(w2.Wrap(okHandler()), target, "198.51.100.10:1", nil); rec.Code != http.StatusForbidden || !w2.IsBlocked(net.ParseIP("198.51.100.10")) {
			t.Fatalf("%s: %d", target, rec.Code)
		}
	}
}

// A scanner's paths count only when the site says they are not there.
func TestScannerPathsBanOnlyWhenTheSiteDoesNotHaveThem(t *testing.T) {
	w, _, _ := newTestWAF(t, t.TempDir(), nil)
	wordpress := w.Wrap(okHandler())
	for i := 0; i < 20; i++ {
		at(wordpress, "/wp-login.php", "203.0.113.20:1", nil)
	}
	if w.IsBlocked(net.ParseIP("203.0.113.20")) {
		t.Fatal("a WordPress visitor was banned")
	}
	static := w.Wrap(notFound())
	for i, p := range []string{"/wp-login.php", "/.env", "/phpmyadmin/", "/.git/config"} {
		at(static, p, "203.0.113.21:1", nil)
		if w.IsBlocked(net.ParseIP("203.0.113.21")) {
			t.Fatalf("banned after %d", i+1)
		}
	}
	if len(w.Watched()) == 0 || w.Watched()[0].Score != 4 {
		t.Fatalf("%+v", w.Watched())
	}
	at(static, "/admin.php", "203.0.113.21:1", nil)
	if !w.IsBlocked(net.ParseIP("203.0.113.21")) {
		t.Fatal("five misses did not ban")
	}
	e := w.Blocked()[0]
	if !strings.Contains(e.Reason, "banned:") || e.Strikes != 1 {
		t.Fatalf("%+v", e)
	}
}

// Never: this machine, the private ranges (the VPN), the allow list, and
// whoever logged in to the panel lately.
func TestTheHouseAndTheOperatorAreNeverBanned(t *testing.T) {
	w, _, _ := newTestWAF(t, t.TempDir(), nil)
	h := w.Wrap(okHandler())
	for _, remote := range []string{"127.0.0.1:1", "10.77.0.2:1", "10.66.3.4:1"} {
		at(h, "/", remote, map[string]string{"User-Agent": "${jndi:ldap://x}"})
		if w.IsBlocked(net.ParseIP(strings.Split(remote, ":")[0])) {
			t.Fatalf("%s banned", remote)
		}
	}
	op := net.ParseIP("203.0.113.30")
	w.Trust(op, time.Hour)
	for i := 0; i < 10; i++ {
		w.Strike(op, 1, "panel login failed", "panel.test", "/login")
	}
	if w.IsBlocked(op) {
		t.Fatal("the operator was banned")
	}
}

func TestEachBanLastsLongerUpToTheCeiling(t *testing.T) {
	b := (&BanPolicy{}).Effective()
	if b.BanFor(0) != time.Hour || b.BanFor(1) != 4*time.Hour || b.BanFor(9) != 30*24*time.Hour {
		t.Fatalf("%v %v %v", b.BanFor(0), b.BanFor(1), b.BanFor(9))
	}
	w, _, _ := newTestWAF(t, t.TempDir(), nil)
	var out []string
	w.OnUnblock = func(ip net.IP) { out = append(out, ip.String()) }
	_ = w.Block(net.ParseIP("203.0.113.40"), time.Hour, "x")
	_ = w.Unblock(net.ParseIP("203.0.113.40"))
	if len(out) != 1 {
		t.Fatal("the kernel was not told to unblock")
	}
}
