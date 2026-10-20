package security

import (
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/dcimorra/middleware-public/internal/pages"
)

// Site is what the door needs to know about the site a request is for.
type Site struct {
	Domain string
	// Rules are the manifest's; the door compiles them once per pointer,
	// and a reloaded manifest is a new pointer.
	Rules        *Rules
	Title, Theme string
}

// Lookup resolves a host to its site.
type Lookup func(host string) (Site, bool)

// Options configure a Door.
type Options struct {
	Lookup Lookup
	// Host is the site name a request is for (proxy.Host); nil is the
	// Host header without its port.
	Host func(r *http.Request) string
	// ClientIP and Country are the edge's; nil means RemoteAddr and "".
	ClientIP func(r *http.Request) net.IP
	Country  func(r *http.Request, ip net.IP) string
	Now      func() time.Time
}

// Door is the hook. It sits after the WAF and before access: comparing
// two lists is cheaper than checking a password, and whoever is not
// getting in does not need to be shown the box.
type Door struct {
	opts Options

	mu       sync.Mutex
	compiled map[string]compiled // by domain
	recent   map[string][]Block  // by domain, newest last
}

type compiled struct {
	rules  *Rules
	filter *Filter
}

// Block is one refused request, for the panel: what it claimed to be, so
// a rule can be tuned by looking at what it does.
type Block struct {
	At      time.Time `json:"at"`
	IP      string    `json:"ip"`
	Reason  string    `json:"reason"`
	What    string    `json:"what"`
	Path    string    `json:"path"`
	Agent   string    `json:"agent,omitempty"`
	Country string    `json:"country,omitempty"`
}

// keep is how many refusals are kept per site.
const keep = 50

// New returns a door.
func New(opts Options) *Door {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Host == nil {
		opts.Host = hostOf
	}
	return &Door{opts: opts, compiled: map[string]compiled{}, recent: map[string][]Block{}}
}

// filter is the compiled rules of a site. Rules that do not compile do
// not reach here (the manifest is refused), but if they did, the house's
// paths would still be cut and the rest would let everyone in, which is
// what this door being the softest one means.
func (d *Door) filter(s Site) *Filter {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.compiled[s.Domain]; ok && c.rules == s.Rules {
		return c.filter
	}
	f, err := Compile(s.Rules)
	if err != nil {
		f, _ = Compile(nil)
	}
	d.compiled[s.Domain] = compiled{rules: s.Rules, filter: f}
	return f
}

// Wrap implements the hook.
func (d *Door) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site, ok := d.opts.Lookup(d.opts.Host(r))
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		f := d.filter(site)
		if v := f.DecidePath(r.URL.Path); !v.Pass {
			d.record(site.Domain, r, v, "")
			// HTML, not plain text: a URL called id_rsa with a text body
			// is saved as a file by some browsers, and whoever probed it
			// believes they took something.
			pages.Render(w, r, pages.ForSite("notfound", site.Title, site.Domain, site.Theme))
			return
		}
		if !f.Active() {
			next.ServeHTTP(w, r)
			return
		}
		var ip net.IP
		if d.opts.ClientIP != nil {
			ip = d.opts.ClientIP(r)
		}
		country := ""
		if d.opts.Country != nil {
			country = d.opts.Country(r, ip)
		}
		v := f.Decide(country, r.Header.Get("User-Agent"), d.opts.Now())
		if v.Pass {
			next.ServeHTTP(w, r)
			return
		}
		d.record(site.Domain, r, v, country)
		n := pages.ForSite("refused-"+v.Reason, site.Title, site.Domain, site.Theme)
		switch {
		case f.message != "":
			n.Line = f.message
		case v.Reason == ByBot && f.window != nil:
			// The one rule a visitor is told: a crawler has no other
			// way of knowing that coming back later works, and that is
			// what the window is for.
			n.Line += " " + pages.RobotWindow(f.window.fromText, f.window.toText, f.window.loc.String())
		}
		pages.Render(w, r, n)
	})
}

func (d *Door) record(domain string, r *http.Request, v Verdict, country string) {
	b := Block{At: d.opts.Now().UTC(), Reason: v.Reason, What: v.What, Path: r.URL.Path, Agent: r.Header.Get("User-Agent"), Country: country}
	if d.opts.ClientIP != nil {
		if ip := d.opts.ClientIP(r); ip != nil {
			b.IP = ip.String()
		}
	}
	if b.IP == "" {
		b.IP, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	if len(b.Agent) > 300 {
		b.Agent = b.Agent[:300]
	}
	if len(b.Path) > 300 {
		b.Path = b.Path[:300]
	}
	d.mu.Lock()
	list := append(d.recent[domain], b)
	if len(list) > keep {
		list = append([]Block(nil), list[len(list)-keep:]...)
	}
	d.recent[domain] = list
	d.mu.Unlock()
}

// Recent is the last refusals for a domain, newest first.
func (d *Door) Recent(domain string) []Block {
	d.mu.Lock()
	defer d.mu.Unlock()
	list := d.recent[domain]
	out := make([]Block, len(list))
	for i, b := range list {
		out[len(list)-1-i] = b
	}
	return out
}

func hostOf(r *http.Request) string {
	h := r.Host
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return h
}
