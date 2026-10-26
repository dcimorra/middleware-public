package waf

import (
	"hash/maphash"
	"net"
	"net/http"
	"net/netip"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/middleware-public/internal/manifest"
)

// shard is one slice of the bucket map. Sixty-four shards keep a flood from
// one address from serialising every other site's requests behind one lock.
type shard struct {
	mu sync.Mutex
	m  map[bucketKey]*bucket
}

// bucketKey is one visitor of one site, or, with client zero, the whole
// address on that site.
type bucketKey struct {
	host   string
	addr   netip.Addr
	client uint64
}

type bucket struct {
	tokens float64
	last   time.Time
}

var seed = maphash.MakeSeed()

func (w *WAF) shardFor(k bucketKey) *shard {
	var h maphash.Hash
	h.SetSeed(seed)
	h.WriteString(k.host)
	b := k.addr.As16()
	h.Write(b[:])
	var c [8]byte
	for i := range c {
		c[i] = byte(k.client >> (8 * i))
	}
	h.Write(c[:])
	return &w.buckets[h.Sum64()%uint64(len(w.buckets))]
}

// NATShare is how many visitors one address may stand for on one site: a
// home or an office behind one router. The address as a whole has this
// many times a visitor's rate and burst; only a flood past that ceiling
// counts toward a block.
const NATShare = 8

// limit is what allow decided.
type limit int

const (
	limitOK limit = iota
	// limitClient: this visitor went past its own rate. A 429 and
	// nothing more: a person reloading a heavy page, an app polling.
	limitClient
	// limitAddress: the address as a whole went past NATShare visitors'
	// worth. A flood; it counts toward a block.
	limitAddress
)

// allow charges cost to the visitor's bucket and to its address's on the
// site. A bucket fills at requests/window and holds burst (requests when
// burst is unset), so a page with its assets loads in one go and a
// crawler that never pauses runs dry. The address is charged even when
// the visitor is refused: one that keeps hammering through its 429s is
// what drains the ceiling and gets blocked.
//
// The limit used to be one bucket per address and every 429 counted
// toward a block in the firewall, which closes every site at once: two
// phones on one home connection reloading a page of 67 files blocked the
// house's own operator from all of them.
func (w *WAF) allow(host string, addr netip.Addr, client uint64, rate *manifest.Rate, cost float64) limit {
	window := time.Duration(rate.Window)
	if rate.Requests <= 0 || window <= 0 {
		return limitOK
	}
	burst := float64(rate.Burst)
	if burst <= 0 {
		burst = float64(rate.Requests)
	}
	perSec := float64(rate.Requests) / window.Seconds()
	whole := w.take(bucketKey{host: host, addr: addr}, burst*NATShare, perSec*NATShare, cost)
	// What a page brings in with it is the address's to pay and never
	// the visitor's: refused, a page reloaded a few times came back with
	// half its images missing, which is breaking it for a person who did
	// nothing. A flood of them still drains the address.
	mine := cost < 1 || w.take(bucketKey{host: host, addr: addr, client: client | 1}, burst, perSec, cost)
	switch {
	case !whole:
		return limitAddress
	case !mine:
		return limitClient
	}
	return limitOK
}

// take charges cost to one bucket and says whether it had it.
func (w *WAF) take(k bucketKey, burst, perSec, cost float64) bool {
	now := w.now()
	s := w.shardFor(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[bucketKey]*bucket{}
	}
	b := s.m[k]
	if b == nil {
		b = &bucket{tokens: burst, last: now}
		s.m[k] = b
	} else {
		b.tokens += now.Sub(b.last).Seconds() * perSec
		if b.tokens > burst {
			b.tokens = burst
		}
		b.last = now
	}
	if b.tokens < cost {
		// Refused requests still drain it, down to one burst below
		// empty: a client hammering through its 429s stays refused
		// until it pauses, instead of getting a request in at every
		// refill.
		if b.tokens > -burst {
			b.tokens -= cost
		}
		return false
	}
	b.tokens -= cost
	return true
}

// clientOf tells apart the visitors behind one address: two phones on one
// home connection are two browsers. It is no proof of anything, a script
// can change its User-Agent at will; that is what the address's ceiling
// is for.
func clientOf(r *http.Request) uint64 {
	var h maphash.Hash
	h.SetSeed(seed)
	h.WriteString(r.UserAgent())
	h.WriteByte(0)
	h.WriteString(r.Header.Get("Accept-Language"))
	return h.Sum64()
}

// costOf is what a request takes from the buckets. What a page brings in
// with it (an image, a script, a font) or asks again only to hear it has
// not changed costs a quarter, and only the address pays it (allow): a
// page of sixty files then weighs like fifteen requests, and a client
// asking for pages, an API or a login still pays one each.
func costOf(r *http.Request) float64 {
	if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
		return 0.25
	}
	switch r.Header.Get("Sec-Fetch-Dest") {
	case "image", "script", "style", "font", "audio", "video", "track", "manifest":
		return 0.25
	case "":
		if staticExt[strings.ToLower(path.Ext(r.URL.Path))] {
			return 0.25
		}
	}
	return 1
}

var staticExt = map[string]bool{
	".js": true, ".mjs": true, ".css": true, ".map": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".avif": true, ".svg": true, ".ico": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true,
	".mp3": true, ".mp4": true, ".webm": true, ".ogg": true,
}

// evict drops buckets nobody touched for ten minutes, trip counters older
// than a day and block entries expired for more than a day. It runs from
// Run; tests call it directly.
func (w *WAF) evict() {
	now := w.now()
	for i := range w.buckets {
		s := &w.buckets[i]
		s.mu.Lock()
		for k, b := range s.m {
			if now.Sub(b.last) > 10*time.Minute {
				delete(s.m, k)
			}
		}
		s.mu.Unlock()
	}
	w.trips.Range(func(k, v any) bool {
		t := v.(*tripCounter)
		t.mu.Lock()
		old := now.Sub(t.since) > 24*time.Hour
		t.mu.Unlock()
		if old {
			w.trips.Delete(k)
		}
		return true
	})
	w.mu.Lock()
	changed := false
	for k, e := range w.blocked {
		if e.Until.Add(24 * time.Hour).Before(now) {
			delete(w.blocked, k)
			changed = true
		}
	}
	if changed {
		_ = w.saveBlockedLocked()
	}
	w.mu.Unlock()
}

// tripCounter counts refusals per address within a sliding minute.
type tripCounter struct {
	mu     sync.Mutex
	since  time.Time
	hits   int
	status int
	// floods are the refusals of the address as a whole (limitAddress)
	// in the same minute: the only ones that count toward a block.
	floods int
}

func (w *WAF) counter(ip string) *tripCounter {
	v, _ := w.trips.LoadOrStore(ip, &tripCounter{since: w.now()})
	return v.(*tripCounter)
}

// count records a refusal for the panel's "recent" list.
func (w *WAF) count(ip net.IP, status int) {
	t := w.counter(ip.String())
	now := w.now()
	t.mu.Lock()
	if now.Sub(t.since) >= time.Minute {
		t.since, t.hits, t.floods = now, 0, 0
	}
	t.hits++
	t.status = status
	t.mu.Unlock()
}

// trip is called after a 429 of the address as a whole (limitAddress); a
// visitor's own 429 never comes here. An address that floods Trips times
// in a minute is blocked, and each block lasts twice the previous one (ten
// minutes, twenty, forty… up to a day), because a client that keeps
// hammering after a block is not going to stop by itself.
func (w *WAF) trip(ip net.IP, host, path string) {
	w.mu.RLock()
	trips := w.policy.Trips
	w.mu.RUnlock()
	// An address the operator signed in from this week, the allow list
	// and the private ranges get the 429s and never the block: the block
	// goes to the machine's firewall and closes every site at once, and a
	// page of 67 files reloaded three times in a row was enough to put the
	// operator out of the panel and their own sites for ten minutes, then
	// twenty, then forty. Strikes already spared them; the trips did not.
	if trips <= 0 || w.exempt(ip) {
		return
	}
	t := w.counter(ip.String())
	t.mu.Lock()
	t.floods++
	hits := t.floods
	t.mu.Unlock()
	if hits < trips || w.IsBlocked(ip) {
		return
	}
	w.mu.RLock()
	strikes := 0
	if e := w.blocked[ip.String()]; e != nil {
		strikes = e.Strikes
	}
	w.mu.RUnlock()
	dur := 10 * time.Minute
	for i := 0; i < strikes && dur < 24*time.Hour; i++ {
		dur *= 2
	}
	if dur > 24*time.Hour {
		dur = 24 * time.Hour
	}
	// Where the last refusal went, so the block says who to look at: a
	// tab left in a loop, an app polling a site's API. Without it the
	// operator blocked from home had no way to know what had been asking.
	if len(path) > 120 {
		path = path[:120] + "…"
	}
	_ = w.block(ip, dur, "flooded the rate limit "+strconv.Itoa(hits)+" times in a minute (last: "+host+path+")", "waf", true)
}
