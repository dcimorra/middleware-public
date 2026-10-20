// Package edge decides which hop in front of the house is believed about the
// client: who it is (CF-Connecting-IP, X-Forwarded-For), where it is
// (CF-IPCountry) and how it came (X-Forwarded-Proto). There is one answer
// for the whole house, here, because three copies of it (the WAF, the
// login brake, the proxy) said three different things in V2's first draft:
// the WAF believed loopback only, the login believed anyone, and the proxy
// copied whatever the client wrote through to the sites.
package edge

import (
	"net"
	"net/http"
	"strings"
)

// Remote is the address of the hop that opened the connection.
func Remote(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(strings.Trim(host, "[]"))
}

// Trusted reports whether the hop that opened the connection may speak for
// the client. Today that is loopback: the tunnel's connector and any proxy
// on this machine. A client reaching the 80/443 door directly is not, so
// whatever it writes in those headers is ignored and removed.
func Trusted(r *http.Request) bool {
	ip := Remote(r)
	return ip != nil && ip.IsLoopback()
}

// ClientIP is the address a request really came from. From a trusted hop
// the edge's header is believed: CF-Connecting-IP first, then the last
// non-loopback hop of X-Forwarded-For, which is the one the trusted proxy
// itself appended. From anywhere else the headers are ignored, because a
// client can write them.
func ClientIP(r *http.Request) net.IP {
	remote := Remote(r)
	if remote == nil || !remote.IsLoopback() {
		return remote
	}
	if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
		if ip := net.ParseIP(cf); ip != nil {
			return ip
		}
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			if ip := net.ParseIP(strings.TrimSpace(parts[i])); ip != nil && !ip.IsLoopback() {
				return ip
			}
		}
	}
	return remote
}

// Country is the edge's country code for the client, or "" when the hop
// is not trusted to say.
func Country(r *http.Request) string {
	if !Trusted(r) {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(r.Header.Get("CF-IPCountry")))
}

// HTTPS reports whether the client's hop was TLS: this connection, or the
// trusted edge saying so.
func HTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return Trusted(r) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// spoofable are the headers a site may read as "who is the client". From
// an untrusted hop they are the client's own words and are removed before
// the request reaches a site; X-Forwarded-For is rebuilt by the proxy.
var spoofable = []string{
	"CF-Connecting-IP", "CF-Connecting-IPv6", "CF-IPCountry", "CF-Ray", "CF-Visitor",
	"CF-Worker", "True-Client-IP", "X-Real-IP", "X-Forwarded-For", "X-Forwarded-Host",
	"X-Forwarded-Proto", "X-Client-IP", "Forwarded",
}

// Scrub prepares the headers a site will see. From an untrusted hop the
// edge headers are dropped; in every case CF-Connecting-IP and X-Real-IP
// are rewritten to the address this package believes, so a site that
// reads either gets the same answer the house acted on.
func Scrub(in *http.Request, out http.Header) {
	if !Trusted(in) {
		for _, h := range spoofable {
			out.Del(h)
		}
	}
	if ip := ClientIP(in); ip != nil {
		out.Set("CF-Connecting-IP", ip.String())
		out.Set("X-Real-IP", ip.String())
	}
}
