package waf

import (
	"hash/maphash"
	"net"
	"net/netip"
	"strconv"
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

type bucketKey struct {
	host string
	addr netip.Addr
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
	return &w.buckets[h.Sum64()%uint64(len(w.buckets))]
}

// allow takes one token from the (site, address) bucket. The bucket fills
// at requests/window and holds burst tokens (requests when burst is unset),
// so a page with thirty assets loads in one go and a crawler that never
// pauses runs dry.
func (w *WAF) allow(host string, addr netip.Addr, rate *manifest.Rate) bool {
	window := time.Duration(rate.Window)
	if rate.Requests <= 0 || window <= 0 {
		return true
	}
	burst := float64(rate.Burst)
	if burst <= 0 {
		burst = float64(rate.Requests)
	}
	perSec := float64(rate.Requests) / window.Seconds()
	now := w.now()
	k := bucketKey{host: host, addr: addr}
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
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
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
		t.since, t.hits = now, 0
	}
	t.hits++
	t.status = status
	t.mu.Unlock()
}

// trip is called after a 429. An address that trips the limit Trips times
// in a minute is blocked, and each block lasts twice the previous one (ten
// minutes, twenty, forty… up to a day), because a client that keeps
// hammering after a block is not going to stop by itself.
func (w *WAF) trip(ip net.IP) {
	w.mu.RLock()
	trips := w.policy.Trips
	w.mu.RUnlock()
	if trips <= 0 {
		return
	}
	t := w.counter(ip.String())
	t.mu.Lock()
	hits := t.hits
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
	_ = w.block(ip, dur, "rate limit tripped "+strconv.Itoa(hits)+" times in a minute", "waf", true)
}
