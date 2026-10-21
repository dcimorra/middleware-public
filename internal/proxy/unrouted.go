package proxy

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Unrouted records who knocks on a door that does not exist: a name that
// reaches this house and belongs to no site. A tunnel rule left alive
// after its site went, a DNS record pointing here for months, a subdomain
// with one letter too many: answering 404 and keeping quiet leaves each of
// them working just enough that nobody finds out. V1 found eight dead
// tunnel rules on its machine by reading this record.
//
// A name alone does not answer the question one always asks on seeing it
// ("edition.cnn.com? how?"): a rule that outlived its site and a scanner
// calling port 80 with whatever Host it likes look the same. So the door
// it came by is kept too, and a few samples with what decides it. It is
// not an access log: five samples per name, cut short, two hundred names,
// in memory.
type Unrouted struct {
	mu    sync.Mutex
	hosts map[string]*Knock
	now   func() time.Time
}

// Knock is one name nobody serves and somebody asked for.
type Knock struct {
	Host  string    `json:"host"`
	Count int64     `json:"count"`
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
	// Doors counts the calls by the door they came in by.
	Doors   map[string]int64 `json:"doors,omitempty"`
	Samples []KnockSample    `json:"samples,omitempty"`
}

// KnockSample is one call.
type KnockSample struct {
	When   time.Time `json:"when"`
	Door   string    `json:"door,omitempty"`
	Method string    `json:"method"`
	Path   string    `json:"path"`
	IP     string    `json:"ip,omitempty"`
	Agent  string    `json:"agent,omitempty"`
	// Proxy: the request came in absolute form (GET http://host/), which
	// no browser writes: someone testing whether this machine relays
	// other people's traffic, which explains the famous names at once.
	Proxy bool `json:"proxy,omitempty"`
	// TLS and SNI: encrypted, and the name the certificate was asked for.
	// An SNI that is not the Host is another thing browsers do not do.
	TLS bool   `json:"tls,omitempty"`
	SNI string `json:"sni,omitempty"`
}

const (
	maxKnocks  = 200
	maxSamples = 5
)

// NewUnrouted builds an empty record.
func NewUnrouted() *Unrouted {
	return &Unrouted{hosts: map[string]*Knock{}, now: time.Now}
}

// cut trims what the caller wrote: a User-Agent can be kilobytes, and
// control characters end up painted in the panel.
func cut(v string, n int) string {
	v = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(v))
	if len(v) <= n {
		return v
	}
	return v[:n] + "…"
}

// Note records a request for host, which no site serves.
func (u *Unrouted) Note(host, door, ip string, r *http.Request) {
	if u == nil || host == "" {
		return
	}
	s := KnockSample{When: u.now(), Door: door, Method: cut(r.Method, 10), Path: cut(r.URL.Path, 120), IP: ip, Agent: cut(r.UserAgent(), 160),
		Proxy: r.URL.IsAbs() || strings.HasPrefix(r.RequestURI, "http://") || strings.HasPrefix(r.RequestURI, "https://")}
	if r.TLS != nil {
		s.TLS, s.SNI = true, cut(r.TLS.ServerName, 100)
	}
	host = cut(strings.ToLower(host), 120)
	u.mu.Lock()
	defer u.mu.Unlock()
	k := u.hosts[host]
	if k == nil {
		if len(u.hosts) >= maxKnocks {
			// Full: the one seen longest ago says least.
			var oldest string
			var when time.Time
			for h, x := range u.hosts {
				if oldest == "" || x.Last.Before(when) {
					oldest, when = h, x.Last
				}
			}
			delete(u.hosts, oldest)
		}
		k = &Knock{Host: host, First: s.When, Doors: map[string]int64{}}
		u.hosts[host] = k
	}
	k.Count++
	k.Last = s.When
	if door != "" {
		k.Doors[door]++
	}
	k.Samples = append(k.Samples, s)
	if len(k.Samples) > maxSamples {
		k.Samples = k.Samples[len(k.Samples)-maxSamples:]
	}
}

// List is every name, most recent first.
func (u *Unrouted) List() []Knock {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]Knock, 0, len(u.hosts))
	for _, k := range u.hosts {
		c := *k
		c.Doors = map[string]int64{}
		for d, n := range k.Doors {
			c.Doors[d] = n
		}
		c.Samples = append([]KnockSample(nil), k.Samples...)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out
}

// Forget drops a name: dealt with, or not worth looking at again.
func (u *Unrouted) Forget(host string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.hosts[host]; !ok {
		return false
	}
	delete(u.hosts, host)
	return true
}
