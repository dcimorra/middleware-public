package manifest

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Redirect is one rule of a site's redirects: a request that matches From
// is sent to To with Code. Written in the manifest, so the site's folder
// says where its old addresses go, and checked before the site is served:
// a site stopped or behind its holding page still sends them on.
type Redirect struct {
	// From is a path ("/old"), a path and everything under it ("/blog/*"),
	// or a host with either ("old.example.com/*", "old.example.com/x"): a
	// name the site answers to (an alias) sent somewhere else whole.
	From string `json:"from"`
	// To is an address ("https://new.example.com/x") or a path on the same
	// host ("/new"). When both end in "/*", what followed From's prefix
	// follows To's: /blog/a → /articles/a.
	To string `json:"to"`
	// Code is 301 (the default), 302, 303, 307 or 308.
	Code int `json:"code,omitempty"`
}

// Check says what is wrong with the rule, if anything.
func (r Redirect) Check() error {
	from, to := strings.TrimSpace(r.From), strings.TrimSpace(r.To)
	if from == "" || to == "" {
		return errors.New("from and to are needed")
	}
	switch r.Code {
	case 0, 301, 302, 303, 307, 308:
	default:
		return fmt.Errorf("code %d is not 301, 302, 303, 307 or 308", r.Code)
	}
	if strings.Count(from, "*") > 1 || (strings.Contains(from, "*") && !strings.HasSuffix(from, "/*") && from != "*") {
		return fmt.Errorf("from %q: a * goes only at the end, after a /", from)
	}
	if strings.HasPrefix(to, "/") {
		return nil
	}
	u, err := url.Parse(to)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("to %q is neither a path (/new) nor an http(s) address", to)
	}
	return nil
}

// Status is the rule's code, 301 when it names none.
func (r Redirect) Status() int {
	if r.Code == 0 {
		return 301
	}
	return r.Code
}

// split is From's host (empty for any of the site's) and path.
func (r Redirect) split() (host, path string) {
	from := strings.TrimSpace(r.From)
	if strings.HasPrefix(from, "/") || from == "*" {
		return "", from
	}
	from = strings.TrimPrefix(strings.TrimPrefix(from, "https://"), "http://")
	host, path, _ = strings.Cut(from, "/")
	return strings.ToLower(host), "/" + path
}

// Target is where a request for host and path goes by this rule, and
// whether it matches. query is the request's, kept unless To has its own.
func (r Redirect) Target(host, path, query string) (string, bool) {
	rh, rp := r.split()
	if rh != "" && !strings.EqualFold(rh, host) {
		return "", false
	}
	if rp == "/" && rh != "" {
		// "old.example.com" alone is the whole host.
		rp = "/*"
	}
	rest := ""
	switch {
	case rp == "*" || rp == "/*":
		rest = strings.TrimPrefix(path, "/")
	case strings.HasSuffix(rp, "/*"):
		base := strings.TrimSuffix(rp, "/*")
		if path != base && !strings.HasPrefix(path, base+"/") {
			return "", false
		}
		rest = strings.TrimPrefix(strings.TrimPrefix(path, base), "/")
	default:
		if path != rp && path != rp+"/" {
			return "", false
		}
	}
	to := strings.TrimSpace(r.To)
	if strings.HasSuffix(to, "/*") {
		to = strings.TrimSuffix(to, "*") + rest
	}
	if query != "" && !strings.Contains(to, "?") {
		to += "?" + query
	}
	return to, true
}

// RedirectFor is the first rule that matches, in the manifest's order.
func (m *Manifest) RedirectFor(host, path, query string) (string, int, bool) {
	if m == nil {
		return "", 0, false
	}
	for _, r := range m.Redirects {
		if to, ok := r.Target(host, path, query); ok {
			return to, r.Status(), true
		}
	}
	return "", 0, false
}
