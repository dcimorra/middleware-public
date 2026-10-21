// Package domains says whether a name is free under a set of endings, and
// what each costs. Cloudflare's API does not search domains (V1 probed it
// whole: of a domain not yours it answers the same for google.com as for
// one made up), so this asks RDAP: IANA's bootstrap file says which server
// keeps each ending, a 404 there is free and a 200 is taken. Some endings
// have no RDAP server (.es, .io, .eu, .co and .me among them, in 2026);
// for those it falls back to DNS, which cannot tell a free name from one
// registered and not delegated, so the answer says it is not sure.
//
// Prices are written by hand in data/tld-prices.json with the date they
// were read; past six months the answer says they may be old, and an
// ending not in the file has no price, never an approximate one.
package domains

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/middleware-public/internal/fsx"
)

// BootstrapURL is IANA's map of RDAP servers by ending.
const BootstrapURL = "https://data.iana.org/rdap/dns.json"

// Options configure a Searcher.
type Options struct {
	DataDir string
	// Bootstrap overrides BootstrapURL (tests).
	Bootstrap string
	HTTP      *http.Client
	// LookupNS is the DNS fallback; nil uses the system resolver.
	LookupNS func(ctx context.Context, name string) ([]*net.NS, error)
	Now      func() time.Time
}

// Searcher answers searches, keeping IANA's map for a day.
type Searcher struct {
	opts    Options
	mu      sync.Mutex
	servers map[string]string // ending → RDAP base URL
	fetched time.Time
}

// New builds a searcher.
func New(opts Options) *Searcher {
	if opts.Bootstrap == "" {
		opts.Bootstrap = BootstrapURL
	}
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{Timeout: 5 * time.Second}
	}
	if opts.LookupNS == nil {
		opts.LookupNS = net.DefaultResolver.LookupNS
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Searcher{opts: opts}
}

// Result is one ending.
type Result struct {
	Domain string `json:"domain"`
	// Status is "free", "taken" or "unknown".
	Status string `json:"status"`
	// Sure is false when the answer came from DNS, not RDAP.
	Sure bool `json:"sure"`
	// Price is the yearly price in USD from the hand-written table, or 0.
	Price float64 `json:"price,omitempty"`
	Error string  `json:"error,omitempty"`
}

// Prices is data/tld-prices.json.
type Prices struct {
	// Updated is when the prices were read, YYYY-MM-DD.
	Updated string             `json:"updated"`
	USD     map[string]float64 `json:"usd"`
}

// Answer is a whole search.
type Answer struct {
	Results []Result `json:"results"`
	// PricesFrom is when the price table was written; StalePrices says it
	// is over six months old.
	PricesFrom  string `json:"prices_from,omitempty"`
	StalePrices bool   `json:"stale_prices,omitempty"`
}

var labelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ErrBadName is a name that is not one label.
var ErrBadName = errors.New("a name is one label: letters, digits and hyphens")

// Prices is the hand-written price table.
func (s *Searcher) Prices() Prices {
	p := s.prices()
	if p.USD == nil {
		p.USD = map[string]float64{}
	}
	return p
}

// SetPrices writes the table, dated today: the date is what says, six
// months on, that the prices may be old.
func (s *Searcher) SetPrices(usd map[string]float64) (Prices, error) {
	clean := map[string]float64{}
	for tld, v := range usd {
		tld = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(tld), "."))
		if !labelRe.MatchString(tld) || v < 0 {
			return Prices{}, fmt.Errorf("%q: an ending and a price of zero or more", tld)
		}
		clean[tld] = v
	}
	p := Prices{Updated: s.opts.Now().UTC().Format("2006-01-02"), USD: clean}
	return p, fsx.WriteJSON(filepath.Join(s.opts.DataDir, "tld-prices.json"), p, 0o644)
}

func (s *Searcher) prices() Prices {
	var p Prices
	_ = fsx.ReadJSON(filepath.Join(s.opts.DataDir, "tld-prices.json"), &p)
	return p
}

// bootstrap loads IANA's map, cached for a day.
func (s *Searcher) bootstrap(ctx context.Context) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.servers != nil && s.opts.Now().Sub(s.fetched) < 24*time.Hour {
		return s.servers
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.opts.Bootstrap, nil)
	if err != nil {
		return s.servers
	}
	resp, err := s.opts.HTTP.Do(req)
	if err != nil {
		return s.servers // keep yesterday's, if any
	}
	defer resp.Body.Close()
	var doc struct {
		Services [][][]string `json:"services"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&doc) != nil {
		return s.servers
	}
	m := map[string]string{}
	for _, svc := range doc.Services {
		if len(svc) != 2 || len(svc[1]) == 0 {
			continue
		}
		for _, tld := range svc[0] {
			m[strings.ToLower(tld)] = strings.TrimSuffix(svc[1][0], "/") + "/"
		}
	}
	s.servers, s.fetched = m, s.opts.Now()
	return m
}

// Search asks about name under each ending, sixteen at a time.
func (s *Searcher) Search(ctx context.Context, name string, tlds []string) (Answer, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !labelRe.MatchString(name) {
		return Answer{}, ErrBadName
	}
	servers := s.bootstrap(ctx)
	prices := s.prices()
	out := Answer{Results: make([]Result, len(tlds)), PricesFrom: prices.Updated}
	if t, err := time.Parse("2006-01-02", prices.Updated); err == nil && s.opts.Now().Sub(t) > 182*24*time.Hour {
		out.StalePrices = true
	}
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for i, tld := range tlds {
		tld = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(tld), "."))
		out.Results[i] = Result{Domain: name + "." + tld, Status: "unknown", Price: prices.USD[tld]}
		wg.Add(1)
		go func(r *Result, tld string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if base, ok := servers[tld]; ok {
				s.rdap(c, base, r)
				return
			}
			s.dns(c, r)
		}(&out.Results[i], tld)
	}
	wg.Wait()
	sort.SliceStable(out.Results, func(i, j int) bool { return out.Results[i].Status == "free" && out.Results[j].Status != "free" })
	return out, nil
}

func (s *Searcher) rdap(ctx context.Context, base string, r *Result) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"domain/"+r.Domain, nil)
	if err != nil {
		r.Error = err.Error()
		return
	}
	req.Header.Set("Accept", "application/rdap+json")
	resp, err := s.opts.HTTP.Do(req)
	if err != nil {
		r.Error = err.Error()
		return
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		r.Status, r.Sure = "taken", true
	case http.StatusNotFound:
		r.Status, r.Sure = "free", true
	default:
		r.Error = "rdap answered " + resp.Status
	}
}

// dns is the fallback: name servers mean registered; none proves nothing,
// because a name can be registered and not delegated.
func (s *Searcher) dns(ctx context.Context, r *Result) {
	ns, err := s.opts.LookupNS(ctx, r.Domain)
	if err == nil && len(ns) > 0 {
		r.Status, r.Sure = "taken", true
		return
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		r.Status, r.Sure = "free", false
		return
	}
	if err != nil {
		r.Error = err.Error()
	}
}
