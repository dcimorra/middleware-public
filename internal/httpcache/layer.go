package httpcache

import (
	"net/http"
	"path"
	"strconv"

	"time"
)

// Site is what the layer needs about the site a request is for.
type Site struct {
	Domain string
	Rules  []Rule
	// MaxBytes caps this site; it can only ask for less than the house
	// budget, never reserve more.
	MaxBytes int64
	// Generation names the instance answering; when it changes the site
	// is emptied (a handover, a deploy by pointer that touched nothing
	// else).
	Generation string
}

// Wrap is the hook. lookup resolves the request's site; host is the name.
func (c *Cache) Wrap(next http.Handler, lookup func(r *http.Request) (Site, bool)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site, ok := lookup(r)
		if !ok || len(site.Rules) == 0 || (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.Header.Get("Authorization") != "" {
			next.ServeHTTP(w, r)
			return
		}
		rule, ok := Match(site.Rules, path.Clean("/"+r.URL.Path))
		if !ok || rule.Mode == ModeNever {
			next.ServeHTTP(w, r)
			return
		}
		k := key(r)
		for attempt := 0; attempt < 2; attempt++ {
			if c.serveHit(w, r, site, k) {
				return
			}
			// Simultaneous misses on one key reach the site as one: the
			// first goes, the rest wait for it and then read what it kept.
			// Without this the first visit to an expensive page with the
			// cache empty is fifty requests to the process at once.
			c.mu.Lock()
			f := c.flights[site.Domain+" "+k]
			if f == nil {
				f = &flight{done: make(chan struct{})}
				c.flights[site.Domain+" "+k] = f
				c.mu.Unlock()
				defer func() {
					c.mu.Lock()
					delete(c.flights, site.Domain+" "+k)
					c.mu.Unlock()
					close(f.done)
				}()
				break
			}
			c.mu.Unlock()
			select {
			case <-f.done:
			case <-r.Context().Done():
				return
			}
		}
		c.miss(site.Domain)
		w.Header().Set("X-Middleware-Cache", "miss")
		rec := &recorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if r.Method != http.MethodGet || rec.tooBig {
			return
		}
		ttl := ttlFor(rule, w.Header(), rec.status)
		if ttl <= 0 {
			return
		}
		c.keep(site, k, r, w.Header(), rec.status, rec.buf.Bytes(), ttl)
	})
}

func (c *Cache) miss(domain string) {
	c.mu.Lock()
	c.siteLocked(domain).misses++
	c.mu.Unlock()
}

// serveHit answers from the store if it can.
func (c *Cache) serveHit(w http.ResponseWriter, r *http.Request, site Site, k string) bool {
	now := c.now()
	c.mu.Lock()
	s := c.siteLocked(site.Domain)
	if s.generation != site.Generation {
		// A new instance answers now: what is kept is the old one's.
		c.sites[site.Domain] = &store{entries: map[string][]*entry{}, generation: site.Generation, limit: site.MaxBytes}
		s = c.sites[site.Domain]
	}
	s.limit = site.MaxBytes
	var hit *entry
	list := s.entries[k]
	for i := 0; i < len(list); i++ {
		e := list[i]
		if now.After(e.expires) {
			s.remove(k, i)
			list = s.entries[k]
			i--
			continue
		}
		if e.fits(r) {
			hit = e
			break
		}
	}
	if hit == nil {
		c.mu.Unlock()
		return false
	}
	hit.used = now
	s.hits++
	c.mu.Unlock()
	for h, vs := range hit.header {
		w.Header()[h] = append([]string(nil), vs...)
	}
	w.Header().Set("X-Middleware-Cache", "hit")
	w.Header().Set("Age", strconv.Itoa(int(now.Sub(hit.stored).Seconds())))
	w.WriteHeader(hit.status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(hit.body)
	}
	return true
}

// skipHeaders are not kept with an entry: they describe one connection
// or one answer, not the content.
var skipHeaders = map[string]bool{"Date": true, "Connection": true, "Keep-Alive": true, "Transfer-Encoding": true, "X-Middleware-Cache": true, "Age": true}

func (c *Cache) keep(site Site, k string, r *http.Request, h http.Header, status int, body []byte, ttl time.Duration) {
	now := c.now()
	e := &entry{status: status, header: http.Header{}, body: append([]byte(nil), body...), stored: now, expires: now.Add(ttl), used: now, vary: map[string]string{}}
	size := int64(len(body))
	for name, vs := range h {
		if skipHeaders[name] {
			continue
		}
		e.header[name] = append([]string(nil), vs...)
		for _, v := range vs {
			size += int64(len(name) + len(v) + 4)
		}
	}
	for _, v := range varyOf(h) {
		e.vary[v] = r.Header.Get(v)
	}
	e.size = size
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.siteLocked(site.Domain)
	if s.generation != site.Generation {
		return
	}
	list := s.entries[k]
	for i, old := range list {
		if sameVary(old.vary, e.vary) {
			s.bytes -= old.size
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	s.entries[k] = append(list, e)
	s.bytes += size
	for s.limit > 0 && s.bytes > s.limit {
		s.evictOne()
	}
	c.trimLocked()
}

func sameVary(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
