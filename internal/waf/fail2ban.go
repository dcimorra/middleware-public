package waf

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/security"
)

// The bans: fail2ban inside the house. A request that tries to exploit
// something, or keeps asking for what scanners ask for, earns its address
// strikes; enough of them inside the window and the address is blocked,
// here and, through the helper, in the kernel, for longer at every repeat.
//
// What counts is decided by the request and, for paths a real site may
// have (/wp-login.php on a WordPress), by the answer: only an error counts,
// so a site that really serves the path is never the reason its visitors
// are banned. What is an exploit on any site (Log4Shell's lookup, a climb
// out of the folder to /etc/passwd) is refused at once and bans at once.

// BanPolicy is the bans' part of the policy. Zero fields are defaults.
type BanPolicy struct {
	// Off turns the bans off; they are on by default.
	Off bool `json:"off,omitempty"`
	// MaxRetry strikes inside FindMinutes ban the address.
	MaxRetry    int `json:"max_retry,omitempty"`
	FindMinutes int `json:"find_minutes,omitempty"`
	// BanMinutes is the first ban; each repeat multiplies it by Factor (1:
	// always the same), up to MaxBanDays.
	BanMinutes int `json:"ban_minutes,omitempty"`
	Factor     int `json:"factor,omitempty"`
	MaxBanDays int `json:"max_ban_days,omitempty"`
}

// Effective fills the defaults.
func (b *BanPolicy) Effective() BanPolicy {
	out := BanPolicy{MaxRetry: 5, FindMinutes: 10, BanMinutes: 60, Factor: 4, MaxBanDays: 30}
	if b == nil {
		return out
	}
	out.Off = b.Off
	if b.MaxRetry > 0 {
		out.MaxRetry = b.MaxRetry
	}
	if b.FindMinutes > 0 {
		out.FindMinutes = b.FindMinutes
	}
	if b.BanMinutes > 0 {
		out.BanMinutes = b.BanMinutes
	}
	if b.Factor > 0 {
		out.Factor = b.Factor
	}
	if b.MaxBanDays > 0 {
		out.MaxBanDays = b.MaxBanDays
	}
	return out
}

// Validate rejects what cannot be applied.
func (b *BanPolicy) Validate() error {
	if b == nil {
		return nil
	}
	switch {
	case b.MaxRetry < 0 || b.MaxRetry > 1000:
		return errors.New("max_retry: 1 to 1000")
	case b.FindMinutes < 0 || b.FindMinutes > 7*24*60:
		return errors.New("find_minutes: 1 to a week")
	case b.BanMinutes < 0 || b.BanMinutes > 366*24*60:
		return errors.New("ban_minutes: 1 to a year")
	case b.Factor < 0 || b.Factor > 100:
		return errors.New("factor: 1 to 100")
	case b.MaxBanDays < 0 || b.MaxBanDays > 366:
		return errors.New("max_ban_days: 1 to 366")
	}
	return nil
}

// BanFor is how long the next ban of an address with strikes earlier bans
// lasts.
func (b BanPolicy) BanFor(strikes int) time.Duration {
	d := float64(b.BanMinutes) * math.Pow(float64(max(1, b.Factor)), float64(strikes))
	if limit := float64(b.MaxBanDays) * 24 * 60; d > limit {
		d = limit
	}
	return time.Duration(d) * time.Minute
}

// Offense is one strike, for the panel.
type Offense struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
	Host   string    `json:"host,omitempty"`
	Path   string    `json:"path,omitempty"`
}

// Watched is an address with strikes and no ban yet.
type Watched struct {
	IP       string    `json:"ip"`
	Score    int       `json:"score"`
	First    time.Time `json:"first"`
	Offenses []Offense `json:"offenses"`
}

type offender struct {
	mu       sync.Mutex
	first    time.Time
	score    int
	offenses []Offense
}

// exempt are never banned: this machine, the private ranges (the VPN's,
// the mesh's, a LAN's: whoever is there came in another way) and the
// allow list; and an address the operator logged in from lately.
func (w *WAF) exempt(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap()
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsUnspecified() || netip.MustParsePrefix("100.64.0.0/10").Contains(a) {
		return true
	}
	w.mu.RLock()
	allow := w.compiled.allow
	w.mu.RUnlock()
	for _, p := range allow {
		if p.Contains(a) {
			return true
		}
	}
	if v, ok := w.trusted.Load(a.String()); ok && v.(time.Time).After(w.now()) {
		return true
	}
	return false
}

// Trust exempts ip from strikes for d: the operator just logged in from
// it, and a typo on the next login must not lock them out.
func (w *WAF) Trust(ip net.IP, d time.Duration) {
	if a, ok := netip.AddrFromSlice(ip); ok {
		w.trusted.Store(a.Unmap().String(), w.now().Add(d))
		w.offenders.Delete(a.Unmap().String())
	}
}

// Strike counts one offense of ip, of weight strikes, and bans it when the
// count reaches the policy's. It returns whether that happened.
func (w *WAF) Strike(ip net.IP, weight int, reason, host, path string) bool {
	if ip == nil || weight <= 0 {
		return false
	}
	w.mu.RLock()
	bp := w.policy.Ban.Effective()
	w.mu.RUnlock()
	if bp.Off || w.exempt(ip) || w.IsBlocked(ip) {
		return false
	}
	key := ip.String()
	now := w.now()
	v, _ := w.offenders.LoadOrStore(key, &offender{first: now})
	o := v.(*offender)
	o.mu.Lock()
	if now.Sub(o.first) > time.Duration(bp.FindMinutes)*time.Minute {
		o.first, o.score, o.offenses = now, 0, nil
	}
	o.score += weight
	if len(path) > 200 {
		path = path[:200] + "…"
	}
	o.offenses = append(o.offenses, Offense{At: now, Reason: reason, Host: host, Path: path})
	if len(o.offenses) > 10 {
		o.offenses = o.offenses[len(o.offenses)-10:]
	}
	reached := o.score >= bp.MaxRetry
	var what []string
	for _, off := range o.offenses {
		what = append(what, off.Reason+" "+off.Path)
	}
	o.mu.Unlock()
	if !reached {
		return false
	}
	w.offenders.Delete(key)
	w.mu.RLock()
	strikes := 0
	if e := w.blocked[key]; e != nil {
		strikes = e.Strikes
	}
	w.mu.RUnlock()
	msg := fmt.Sprintf("banned: %s", strings.Join(lastN(what, 3), "; "))
	return w.block(ip, bp.BanFor(strikes), msg, "fail2ban", true) == nil
}

func lastN(s []string, n int) []string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// Watched lists the addresses with strikes inside their window, worst
// first.
func (w *WAF) Watched() []Watched {
	w.mu.RLock()
	find := time.Duration(w.policy.Ban.Effective().FindMinutes) * time.Minute
	w.mu.RUnlock()
	now := w.now()
	out := []Watched{}
	w.offenders.Range(func(k, v any) bool {
		o := v.(*offender)
		o.mu.Lock()
		if now.Sub(o.first) <= find && o.score > 0 {
			out = append(out, Watched{IP: k.(string), Score: o.score, First: o.first, Offenses: append([]Offense(nil), o.offenses...)})
		}
		o.mu.Unlock()
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}

// pruneOffenders forgets windows that closed.
func (w *WAF) pruneOffenders() {
	w.mu.RLock()
	find := time.Duration(w.policy.Ban.Effective().FindMinutes) * time.Minute
	w.mu.RUnlock()
	now := w.now()
	w.offenders.Range(func(k, v any) bool {
		o := v.(*offender)
		o.mu.Lock()
		old := now.Sub(o.first) > find
		o.mu.Unlock()
		if old {
			w.offenders.Delete(k)
		}
		return true
	})
	w.trusted.Range(func(k, v any) bool {
		if v.(time.Time).Before(now) {
			w.trusted.Delete(k)
		}
		return true
	})
}

// exploitMarks are in no honest request to any site; one of them is a
// ban. Matched lower-cased against the decoded target and the headers an
// exploit rides in.
var exploitMarks = []struct{ mark, name string }{
	{"${jndi:", "log4shell"},
	{"${${", "log4shell"},
	{"/etc/passwd", "traversal to /etc/passwd"},
	{"/etc/shadow", "traversal to /etc/shadow"},
	{"/proc/self/environ", "traversal to /proc/self/environ"},
	{"php://input", "php wrapper"},
	{"php://filter", "php wrapper"},
	{"allow_url_include", "php remote include"},
	{"auto_prepend_file", "php remote include"},
	{"eval-stdin.php", "phpunit rce"},
	{"/cgi-bin/luci/;stok=", "router rce"},
	{"/boaform/admin/formlogin", "router rce"},
	{"/gponform/diag_form", "router rce"},
	{"/shell?cd+/tmp", "router rce"},
	{";wget http", "command injection"},
	{"|wget http", "command injection"},
	{";curl http", "command injection"},
	{"$(wget", "command injection"},
	{"chmod+777", "command injection"},
	{"/bin/sh -c", "command injection"},
}

// exploitOf says which exploit a request carries, or "".
func exploitOf(r *http.Request) string {
	target := strings.ToLower(r.URL.RawPath + r.URL.Path + "?" + r.URL.RawQuery)
	if dec, err := url.QueryUnescape(target); err == nil {
		target += " " + strings.ToLower(dec)
	}
	for _, m := range exploitMarks {
		if strings.Contains(target, m.mark) {
			return m.name
		}
	}
	// A climb above the site's root: a client that means it sends the dots
	// encoded, and a browser never sends them at all.
	if strings.Contains(target, "../..") || strings.Contains(target, "..%2f") || strings.Contains(target, "%2e%2e") || strings.Contains(target, "..\\") {
		return "path traversal"
	}
	for _, h := range []string{"User-Agent", "Referer", "X-Api-Version", "X-Forwarded-For", "Cookie", "Accept-Language"} {
		v := strings.ToLower(r.Header.Get(h))
		if v == "" {
			continue
		}
		if strings.Contains(v, "${jndi:") || strings.Contains(v, "${${") {
			return "log4shell"
		}
		if strings.HasPrefix(strings.TrimSpace(v), "() {") {
			return "shellshock"
		}
	}
	return ""
}

// scannerPaths are what scanners ask every server for. A site may really
// have some (a WordPress has /wp-login.php), so they count only when the
// answer is an error.
var scannerPaths = []string{
	"/wp-login.php", "/xmlrpc.php", "/wp-admin", "/wp-includes/", "/wp-content/plugins/", "/wp-config",
	"/phpmyadmin", "/pma/", "/myadmin", "/mysqladmin", "/adminer", "/dbadmin",
	"/cgi-bin/", "/hnap1", "/actuator", "/solr/", "/manager/html", "/jmx-console", "/web-console",
	"/owa/", "/ecp/", "/autodiscover/", "/remote/fgt_lang", "/dana-na/", "/+cscoe+/", "/vpn/index.html",
	"/server-status", "/.ds_store", "/.vscode/", "/.idea/", "/vendor/phpunit", "/telescope/",
	"/_ignition/", "/debug/default/view", "/console/", "/geoserver/", "/sdk/weblanguage",
	"/backup.sql", "/dump.sql", "/db.sql", "/database.sql", "/backup.zip", "/www.zip", "/site.zip",
	"/config.json", "/config.php", "/info.php", "/phpinfo.php", "/shell.php", "/cmd.php", "/upload.php",
}

// scannerOf is the scanner pattern a path matches, or "".
func scannerOf(p string) string {
	lp := strings.ToLower(p)
	if probe := security.ProbeOf(p); probe != "" {
		return probe
	}
	for _, s := range scannerPaths {
		if strings.HasPrefix(lp, s) || (strings.HasSuffix(s, "/") && lp == strings.TrimSuffix(s, "/")) {
			return s
		}
	}
	for _, ext := range []string{".php", ".asp", ".aspx", ".jsp", ".cgi", ".pl", ".env", ".bak", ".old", ".sql"} {
		if strings.HasSuffix(lp, ext) {
			return "*" + ext
		}
	}
	return ""
}

// statusOf records the status of an answer, keeping what a handler may
// ask the writer for (flushing, hijacking) reachable.
type statusOf struct {
	http.ResponseWriter
	code int
}

func (s *statusOf) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusOf) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusOf) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusOf) Unwrap() http.ResponseWriter { return s.ResponseWriter }
