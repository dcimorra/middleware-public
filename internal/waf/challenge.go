package waf

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/pages"
)

// clearanceCookie is the name of the cookie a browser earns by coming back.
const clearanceCookie = "mw_clearance"

// clearanceTTL is how long one clearance lasts: short enough that a stolen
// cookie is worth little, long enough that a visitor is not challenged on
// every page.
const clearanceTTL = time.Hour

// challenge answers with the holding page, the cookie and a Refresh header,
// so any browser comes back two seconds later carrying the cookie and is
// let through. No JavaScript is involved, which is the point: a flood tool
// that discards Set-Cookie never gets past this page, and the page is far
// cheaper than anything the site process would do. The cookie is bound to
// the client address and signed with a key made at boot, so a restart
// invalidates every clearance; that is fine, the challenge is for
// emergencies, not sessions.
func (w *WAF) challenge(rw http.ResponseWriter, r *http.Request, ip net.IP) {
	exp := w.now().Add(clearanceTTL).Unix()
	http.SetCookie(rw, &http.Cookie{
		Name:     clearanceCookie,
		Value:    w.sign(ip, exp),
		Path:     "/",
		MaxAge:   int(clearanceTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
	})
	rw.Header().Set("Refresh", "2")
	w.count(ip, http.StatusServiceUnavailable)
	pages.Write(rw, r, "underattack", http.StatusServiceUnavailable)
}

// cleared reports whether the request carries a valid clearance.
func (w *WAF) cleared(r *http.Request, ip net.IP) bool {
	c, err := r.Cookie(clearanceCookie)
	if err != nil {
		return false
	}
	expStr, _, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || exp < w.now().Unix() {
		return false
	}
	return hmac.Equal([]byte(w.sign(ip, exp)), []byte(c.Value))
}

func (w *WAF) sign(ip net.IP, exp int64) string {
	mac := hmac.New(sha256.New, w.key)
	mac.Write(ip.To16())
	mac.Write([]byte{0})
	mac.Write([]byte(strconv.FormatInt(exp, 10)))
	return strconv.FormatInt(exp, 10) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
