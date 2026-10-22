// Package waf is layer 7 in front of everything: the first thing a request
// meets and the last place it can be refused before a site process spends
// anything on it. It is always on, with a global policy on disk and a
// per-site section in the manifest merged over it.
//
// Per request, in this order: client address, allow list (bypasses all of
// what follows), block list, rate limit, body limit, user-agent and country
// policy. The order is deliberate: the cheap checks that end most abusive
// traffic come first. The under-attack challenge is its own hook
// (internal/attack) right after this one, so a flood is rate-limited
// before it can use the challenge page itself as a lever; the global
// switch for it lives in this policy.
//
// Country lookups are not built in. The Country field takes a function;
// the house wires the edge's CF-IPCountry header (internal/edge, believed
// only from a trusted hop), and a GeoIP module can replace it. With no
// function every request has country "" and country rules never match.
package waf

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/middleware-public/internal/attack"
	"github.com/dcimorra/middleware-public/internal/edge"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/pages"
)

// Policy is the global layer-7 policy, data/waf/policy.json. A site's
// manifest can tighten or loosen rate, max_body and under_attack; the lists
// are global only, because a deny list that differs per site is a thing
// nobody can audit.
type Policy struct {
	Rate    *manifest.Rate `json:"rate,omitempty"`
	MaxBody string         `json:"max_body,omitempty"`
	// Attack is under-attack mode for every site and the panel.
	Attack *attack.Rules `json:"attack,omitempty"`
	// UnderAttack is the switch before Attack existed, read as Attack on
	// with its defaults; SetAttack writes Attack and clears it.
	UnderAttack bool `json:"under_attack,omitempty"`
	// DenyCountries are ISO 3166-1 alpha-2 codes, upper case.
	DenyCountries []string `json:"deny_countries,omitempty"`
	// DenyAgents are substrings matched case-insensitively against User-Agent.
	DenyAgents []string `json:"deny_agents,omitempty"`
	// AllowAgents are substrings of crawlers let through the
	// under-attack challenge.
	AllowAgents []string `json:"allow_agents,omitempty"`
	// Allow are CIDRs (or single addresses) that bypass everything.
	Allow []string `json:"allow,omitempty"`
	// Trips is how many rate-limit hits in one minute block an address.
	Trips int `json:"trips,omitempty"`
	// Ban is the fail2ban part (fail2ban.go): on unless Off.
	Ban *BanPolicy `json:"ban,omitempty"`
}

// DefaultPolicy is what a fresh installation runs with: generous enough for
// a busy site behind one NAT, tight enough that a naive flood is blocked.
func DefaultPolicy() Policy {
	return Policy{
		Rate:    &manifest.Rate{Requests: 600, Window: manifest.Duration(time.Minute), Burst: 100},
		MaxBody: "20m",
		Trips:   20,
	}
}

// Validate rejects a policy the WAF could not apply.
func (p Policy) Validate() error {
	if p.Rate != nil && (p.Rate.Requests <= 0 || p.Rate.Window <= 0) {
		return errors.New("rate: requests and window must be positive")
	}
	if p.MaxBody != "" {
		if _, err := ParseSize(p.MaxBody); err != nil {
			return fmt.Errorf("max_body: %w", err)
		}
	}
	for _, c := range p.Allow {
		if _, err := parsePrefix(c); err != nil {
			return fmt.Errorf("allow: %w", err)
		}
	}
	for _, c := range p.DenyCountries {
		if len(c) != 2 {
			return fmt.Errorf("deny_countries: %q is not a two-letter code", c)
		}
	}
	if err := p.Ban.Validate(); err != nil {
		return fmt.Errorf("ban.%w", err)
	}
	if err := p.Attack.Validate(); err != nil {
		return fmt.Errorf("attack.%w", err)
	}
	if p.Trips < 0 {
		return errors.New("trips: must not be negative")
	}
	return nil
}

// Entry is one blocked address.
type Entry struct {
	IP     string    `json:"ip"`
	Until  time.Time `json:"until"`
	Reason string    `json:"reason,omitempty"`
	// Strikes counts the automatic blocks this address earned, so each
	// new one lasts longer than the last.
	Strikes int `json:"strikes,omitempty"`
}

// Hit is a recent offender for the panel.
type Hit struct {
	IP     string `json:"ip"`
	Hits   int    `json:"hits"`
	Status int    `json:"status"`
}

// Site is what the WAF needs to know about the site a request is for.
type Site struct {
	Domain string
	Dir    string
	WAF    *manifest.WAF
}

// Lookup resolves a host to its site. Unknown hosts get the global policy.
type Lookup func(host string) (Site, bool)

// Options configure a WAF.
type Options struct {
	// DataDir is data/; the WAF keeps data/waf/policy.json and blocked.json.
	DataDir string
	Bus     *events.Bus
	Lookup  Lookup
	// Country resolves a request's country code; nil means "".
	Country func(r *http.Request, ip net.IP) string
	// Now is for tests.
	Now func() time.Time
}

// WAF is the layer-7 filter.
type WAF struct {
	opts Options
	now  func() time.Time

	mu       sync.RWMutex
	policy   Policy
	compiled compiledPolicy
	blocked  map[string]*Entry
	// blockedMod is the mtime of blocked.json when it was last read or
	// written, so a block added by the CLI in another process is picked up.
	blockedMod time.Time

	buckets [64]shard
	trips   sync.Map // ip → *tripCounter
	// OnBlock is called after an address is blocked; the privileged helper
	// plugs in layer-3 blocking here. It runs synchronously, keep it fast.
	OnBlock func(ip net.IP, dur time.Duration)
	// OnUnblock is its way back: an address taken off the list here must
	// leave the kernel's too, or it stays cut until the kernel's timer.
	OnUnblock func(ip net.IP)

	// offenders are the addresses with strikes (fail2ban.go); trusted the
	// ones the operator logged in from.
	offenders sync.Map
	trusted   sync.Map
}

type compiledPolicy struct {
	allow         []netip.Prefix
	maxBody       int64
	denyAgents    []string
	allowAgents   []string
	denyCountries map[string]bool
}

// New loads the policy and the block list from DataDir. Missing files mean
// defaults. A broken policy file yields a WAF on defaults plus a
// *PolicyError the caller must surface; a broken block list is an error,
// because silently forgetting who was blocked is worse than not starting.
func New(opts Options) (*WAF, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	w := &WAF{opts: opts, now: opts.Now, blocked: map[string]*Entry{}}
	var policyErr error
	p := DefaultPolicy()
	if err := fsx.ReadJSON(w.policyPath(), &p); err != nil && !fsx.IsNotExist(err) {
		policyErr, p = err, DefaultPolicy()
	}
	if err := w.setPolicy(p); err != nil {
		policyErr = fmt.Errorf("%s: %w", w.policyPath(), err)
		_ = w.setPolicy(DefaultPolicy())
	}
	if policyErr != nil {
		policyErr = &PolicyError{Err: policyErr}
	}
	if err := w.loadBlocked(); err != nil {
		return nil, err
	}
	return w, policyErr
}

// PolicyError says policy.json could not be used. New still returns a
// working WAF with the default policy alongside it: a typo in the policy
// file must be loud (an event, a log line) but must not leave every site
// without a WAF, and must not stop the supervisor from starting.
type PolicyError struct{ Err error }

func (e *PolicyError) Error() string { return "waf policy: " + e.Err.Error() }
func (e *PolicyError) Unwrap() error { return e.Err }

func (w *WAF) policyPath() string  { return filepath.Join(w.opts.DataDir, "waf", "policy.json") }
func (w *WAF) blockedPath() string { return filepath.Join(w.opts.DataDir, "waf", "blocked.json") }

// Policy returns the global policy.
func (w *WAF) Policy() Policy {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.policy
}

// SetPolicy validates, applies and persists a new global policy.
func (w *WAF) SetPolicy(p Policy) error {
	if err := w.setPolicy(p); err != nil {
		return err
	}
	if err := fsx.WriteJSON(w.policyPath(), p, 0o644); err != nil {
		return err
	}
	w.emit(events.Event{Type: "waf.policy", Actor: "operator", Message: "global policy updated"})
	return nil
}

func (w *WAF) setPolicy(p Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	c := compiledPolicy{denyCountries: map[string]bool{}}
	for _, a := range p.Allow {
		pfx, _ := parsePrefix(a)
		c.allow = append(c.allow, pfx)
	}
	if p.MaxBody != "" {
		c.maxBody, _ = ParseSize(p.MaxBody)
	}
	for _, s := range p.DenyAgents {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			c.denyAgents = append(c.denyAgents, s)
		}
	}
	for _, s := range p.AllowAgents {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			c.allowAgents = append(c.allowAgents, s)
		}
	}
	for _, cc := range p.DenyCountries {
		c.denyCountries[strings.ToUpper(cc)] = true
	}
	w.mu.Lock()
	w.policy = p
	w.compiled = c
	w.mu.Unlock()
	return nil
}

// GlobalAttack is the installation-wide under-attack rules, with the old
// boolean read as the mode on with its defaults.
func (w *WAF) GlobalAttack() *attack.Rules {
	p := w.Policy()
	return Effective(p.Attack, p.UnderAttack)
}

// Effective is the under-attack rules a section means: its own block,
// or, from before it existed, a bare under_attack read as on.
func Effective(rules *attack.Rules, legacy bool) *attack.Rules {
	if rules == nil && legacy {
		return &attack.Rules{On: true}
	}
	return rules
}

// SetAttack changes under-attack mode, globally (domain "") or for one
// site, by applying change to the current rules. The site's is written
// into its manifest, because a decision kept only in memory would not
// survive a restart; the engine picks the change up on its next tick
// without restarting the site.
func (w *WAF) SetAttack(domain string, change func(*attack.Rules)) error {
	if domain == "" {
		p := w.Policy()
		r := cloneRules(Effective(p.Attack, p.UnderAttack))
		change(r)
		p.Attack, p.UnderAttack = r, false
		if err := w.SetPolicy(p); err != nil {
			return err
		}
		w.emit(events.Event{Type: "waf.under_attack", Actor: "operator", Message: attackMsg(r), Data: map[string]any{"enabled": r.On, "until": r.Until}})
		return nil
	}
	if w.opts.Lookup == nil {
		return errors.New("no site lookup configured")
	}
	site, ok := w.opts.Lookup(domain)
	if !ok || site.Dir == "" {
		return fmt.Errorf("%s: no such site", domain)
	}
	m, err := manifest.Load(site.Dir)
	if err != nil {
		return err
	}
	legacy := m.WAF != nil && m.WAF.UnderAttack
	r := cloneRules(Effective(m.Attack, legacy))
	change(r)
	m.Attack = r
	if m.WAF != nil {
		m.WAF.UnderAttack = false
		if *m.WAF == (manifest.WAF{}) {
			m.WAF = nil
		}
	}
	if err := manifest.Save(site.Dir, m); err != nil {
		return err
	}
	w.emit(events.Event{Type: "waf.under_attack", Subject: domain, Actor: "operator", Message: attackMsg(r), Data: map[string]any{"enabled": r.On, "until": r.Until}})
	return nil
}

func cloneRules(r *attack.Rules) *attack.Rules {
	if r == nil {
		return &attack.Rules{}
	}
	c := *r
	c.Skip = append([]string(nil), r.Skip...)
	return &c
}

func attackMsg(r *attack.Rules) string {
	switch {
	case !r.On:
		return "under-attack mode disabled"
	case r.Until != "":
		return "under-attack mode enabled until " + r.Until
	default:
		return "under-attack mode enabled with no end date"
	}
}

// Blocked returns the live block list, soonest to expire first.
func (w *WAF) Blocked() []Entry {
	now := w.now()
	w.mu.RLock()
	out := make([]Entry, 0, len(w.blocked))
	for _, e := range w.blocked {
		if e.Until.After(now) {
			out = append(out, *e)
		}
	}
	w.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Until.Before(out[j].Until) })
	return out
}

// IsBlocked reports whether ip is on the list right now.
func (w *WAF) IsBlocked(ip net.IP) bool {
	w.mu.RLock()
	e, ok := w.blocked[ip.String()]
	w.mu.RUnlock()
	return ok && e.Until.After(w.now())
}

// Block adds ip for dur with a reason. A duration of zero means a day.
func (w *WAF) Block(ip net.IP, dur time.Duration, reason string) error {
	return w.block(ip, dur, reason, "operator", false)
}

func (w *WAF) block(ip net.IP, dur time.Duration, reason, actor string, strike bool) error {
	if ip == nil {
		return errors.New("block: not an address")
	}
	if ip.IsLoopback() {
		// Blocking loopback would block the tunnel, the panel and the CLI
		// in one stroke: every request arrives from there.
		return errors.New("block: refusing to block loopback")
	}
	if dur <= 0 {
		dur = 24 * time.Hour
	}
	key := ip.String()
	w.mu.Lock()
	e := w.blocked[key]
	if e == nil {
		e = &Entry{IP: key}
		w.blocked[key] = e
	}
	if strike {
		e.Strikes++
	}
	e.Until = w.now().Add(dur)
	e.Reason = reason
	err := w.saveBlockedLocked()
	w.mu.Unlock()
	if err != nil {
		return err
	}
	w.emit(events.Event{Type: "waf.blocked", Subject: key, Actor: actor, Level: "warn", Message: reason,
		Data: map[string]any{"ip": key, "until": e.Until, "duration": dur.String()}})
	if w.OnBlock != nil {
		w.OnBlock(ip, dur)
	}
	return nil
}

// Unblock removes ip from the list; a missing entry is not an error.
func (w *WAF) Unblock(ip net.IP) error {
	if ip == nil {
		return errors.New("unblock: not an address")
	}
	key := ip.String()
	w.mu.Lock()
	_, had := w.blocked[key]
	delete(w.blocked, key)
	err := w.saveBlockedLocked()
	w.mu.Unlock()
	if err != nil {
		return err
	}
	if had {
		w.emit(events.Event{Type: "waf.unblocked", Subject: key, Actor: "operator", Data: map[string]any{"ip": key}})
	}
	if w.OnUnblock != nil {
		w.OnUnblock(ip)
	}
	return nil
}

// Recent returns addresses that were refused in the last minute, worst first.
func (w *WAF) Recent() []Hit {
	now := w.now()
	var out []Hit
	w.trips.Range(func(k, v any) bool {
		t := v.(*tripCounter)
		t.mu.Lock()
		if now.Sub(t.since) < time.Minute && t.hits > 0 {
			out = append(out, Hit{IP: k.(string), Hits: t.hits, Status: t.status})
		}
		t.mu.Unlock()
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Hits > out[j].Hits })
	if len(out) > 50 {
		out = out[:50]
	}
	return out
}

type blockedFile struct {
	Blocked []Entry `json:"blocked"`
}

func (w *WAF) loadBlocked() error {
	var f blockedFile
	st, err := os.Stat(w.blockedPath())
	if err != nil {
		if fsx.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := fsx.ReadJSON(w.blockedPath(), &f); err != nil {
		return err
	}
	now := w.now()
	m := map[string]*Entry{}
	for i := range f.Blocked {
		e := f.Blocked[i]
		// Expired entries keep their strikes for a week so a repeat
		// offender's next block is longer, then they are forgotten. A day
		// was shorter than the ban a third strike earns.
		if e.Until.Add(7 * 24 * time.Hour).After(now) {
			m[e.IP] = &e
		}
	}
	w.mu.Lock()
	w.blocked = m
	w.blockedMod = st.ModTime()
	w.mu.Unlock()
	return nil
}

func (w *WAF) saveBlockedLocked() error {
	f := blockedFile{Blocked: make([]Entry, 0, len(w.blocked))}
	for _, e := range w.blocked {
		f.Blocked = append(f.Blocked, *e)
	}
	sort.Slice(f.Blocked, func(i, j int) bool { return f.Blocked[i].IP < f.Blocked[j].IP })
	if err := fsx.WriteJSON(w.blockedPath(), f, 0o644); err != nil {
		return err
	}
	if st, err := os.Stat(w.blockedPath()); err == nil {
		w.blockedMod = st.ModTime()
	}
	return nil
}

// Reload re-reads the block list if another process (the CLI) changed it.
func (w *WAF) Reload() error {
	st, err := os.Stat(w.blockedPath())
	if err != nil {
		if fsx.IsNotExist(err) {
			return nil
		}
		return err
	}
	w.mu.RLock()
	same := st.ModTime().Equal(w.blockedMod)
	w.mu.RUnlock()
	if same {
		return nil
	}
	return w.loadBlocked()
}

func (w *WAF) emit(e events.Event) {
	if w.opts.Bus != nil {
		w.opts.Bus.Emit(e)
	}
}

// ParseSize reads "20m", "512k", "1g" or plain bytes.
func ParseSize(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, errors.New("empty size")
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'k':
		mult, s = 1<<10, s[:len(s)-1]
	case 'm':
		mult, s = 1<<20, s[:len(s)-1]
	case 'g':
		mult, s = 1<<30, s[:len(s)-1]
	}
	s = strings.TrimSuffix(s, "b")
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not a size", s)
	}
	return n * mult, nil
}

func parsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if p, err := netip.ParsePrefix(s); err == nil {
		return p, nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is not an address or CIDR", s)
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// effective is the policy for one request after the site's section is
// merged over the global one.
type effective struct {
	rate    *manifest.Rate
	maxBody int64
}

func (w *WAF) effectiveFor(site *manifest.WAF) effective {
	w.mu.RLock()
	e := effective{rate: w.policy.Rate, maxBody: w.compiled.maxBody}
	w.mu.RUnlock()
	if site == nil {
		return e
	}
	if site.Rate != nil {
		e.rate = site.Rate
	}
	if site.MaxBody != "" {
		if n, err := ParseSize(site.MaxBody); err == nil {
			e.maxBody = n
		}
	}
	return e
}

func hostOf(r *http.Request) string {
	h := r.Host
	if i := strings.LastIndexByte(h, ':'); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// Wrap implements app.Hook.
func (w *WAF) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		ip := edge.ClientIP(r)
		if ip == nil {
			// A RemoteAddr that is not an address is a unix socket (the
			// CLI) or a test; neither is something the WAF should judge.
			next.ServeHTTP(rw, r)
			return
		}
		addr, _ := netip.AddrFromSlice(ip)
		addr = addr.Unmap()
		w.mu.RLock()
		c := w.compiled
		w.mu.RUnlock()
		for _, p := range c.allow {
			if p.Contains(addr) {
				next.ServeHTTP(rw, r)
				return
			}
		}
		if w.IsBlocked(ip) {
			w.refuse(rw, r, ip, "forbidden", http.StatusForbidden)
			return
		}
		host := hostOf(r)
		// An exploit is refused and bans at once; a scanner's path counts
		// when the answer says it is not there (below).
		if what := exploitOf(r); what != "" && !w.exempt(ip) {
			w.Strike(ip, 1<<20, what, host, r.URL.RequestURI())
			w.refuse(rw, r, ip, "forbidden", http.StatusForbidden)
			return
		}
		if scan := scannerOf(r.URL.Path); scan != "" {
			rec := &statusOf{ResponseWriter: rw}
			defer func() {
				if rec.code >= 400 && rec.code != http.StatusTooManyRequests && rec.code < 500 {
					w.Strike(ip, 1, "scanner "+scan, host, r.URL.RequestURI())
				}
			}()
			rw = rec
		}
		var site Site
		if w.opts.Lookup != nil {
			site, _ = w.opts.Lookup(host)
		}
		pol := w.effectiveFor(site.WAF)
		if pol.rate != nil && !w.allow(host, addr, pol.rate) {
			w.refuse(rw, r, ip, "ratelimited", http.StatusTooManyRequests)
			w.trip(ip)
			return
		}
		if pol.maxBody > 0 {
			if r.ContentLength > pol.maxBody {
				w.refuse(rw, r, ip, "toolarge", http.StatusRequestEntityTooLarge)
				return
			}
			if r.Body != nil && r.Body != http.NoBody {
				r.Body = http.MaxBytesReader(rw, r.Body, pol.maxBody)
			}
		}
		ua := strings.ToLower(r.UserAgent())
		if matchesAny(ua, c.denyAgents) {
			w.refuse(rw, r, ip, "forbidden", http.StatusForbidden)
			return
		}
		if len(c.denyCountries) > 0 && w.opts.Country != nil {
			if cc := strings.ToUpper(w.opts.Country(r, ip)); cc != "" && c.denyCountries[cc] {
				w.refuse(rw, r, ip, "forbidden", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(rw, r)
	})
}

func matchesAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func (w *WAF) refuse(rw http.ResponseWriter, r *http.Request, ip net.IP, page string, status int) {
	w.count(ip, status)
	pages.Write(rw, r, page, status)
}

// Run evicts idle buckets and picks up block-list changes made from the
// CLI until ctx ends. It returns at once.
func (w *WAF) Run(ctx context.Context) {
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		n := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = w.Reload()
				n++
				if n%12 == 0 {
					w.evict()
					w.pruneOffenders()
				}
			}
		}
	}()
}

// AllowAgents are the policy's crawler fragments, lower-cased, that the
// under-attack challenge lets through.
func (w *WAF) AllowAgents() []string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return append([]string(nil), w.compiled.allowAgents...)
}
