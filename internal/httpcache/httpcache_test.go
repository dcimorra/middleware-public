package httpcache

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type origin struct {
	calls  atomic.Int64
	header map[string]string
	status int
	delay  time.Duration
}

func (o *origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := o.calls.Add(1)
	if o.delay > 0 {
		time.Sleep(o.delay)
	}
	for k, v := range o.header {
		w.Header().Set(k, v)
	}
	if o.status != 0 {
		w.WriteHeader(o.status)
	}
	fmt.Fprintf(w, "answer %d for %s %s", n, r.URL.Path, r.Header.Get("Accept-Encoding"))
}

func setup(rules []Rule, o *origin) (*Cache, http.Handler, *Site) {
	c := New(64 << 20)
	site := &Site{Domain: "a.example", Rules: rules, Generation: "g1"}
	return c, c.Wrap(o, func(*http.Request) (Site, bool) { return *site, true }), site
}

func get(h http.Handler, path string, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "http://a.example"+path, nil)
	for _, kv := range headers {
		k, v, _ := strings.Cut(kv, ": ")
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestOriginModeKeepsWhatTheSiteMarksAndOnlyThat(t *testing.T) {
	o := &origin{header: map[string]string{"Cache-Control": "public, s-maxage=60, max-age=0"}}
	_, h, _ := setup([]Rule{{Path: "/", Mode: ModeOrigin}}, o)
	first := get(h, "/product/1")
	second := get(h, "/product/1")
	if first.Header().Get("X-Middleware-Cache") != "miss" || second.Header().Get("X-Middleware-Cache") != "hit" || second.Body.String() != first.Body.String() || o.calls.Load() != 1 {
		t.Fatalf("calls %d, %q then %q", o.calls.Load(), first.Header().Get("X-Middleware-Cache"), second.Header().Get("X-Middleware-Cache"))
	}
	// A site that says nothing is not cached in origin mode.
	quiet := &origin{}
	_, h2, _ := setup([]Rule{{Path: "/", Mode: ModeOrigin}}, quiet)
	get(h2, "/x")
	get(h2, "/x")
	if quiet.calls.Load() != 2 {
		t.Fatal("origin mode cached a response the site did not mark")
	}
}

func TestTheWallsAManifestCannotRemove(t *testing.T) {
	cases := map[string]*origin{
		"set-cookie":  {header: map[string]string{"Set-Cookie": "s=1"}},
		"private":     {header: map[string]string{"Cache-Control": "private, max-age=60"}},
		"no-store":    {header: map[string]string{"Cache-Control": "no-store"}},
		"vary-cookie": {header: map[string]string{"Vary": "Cookie"}},
		"vary-star":   {header: map[string]string{"Vary": "*"}},
		"404":         {status: 404},
		"stream":      {header: map[string]string{"Content-Type": "text/event-stream"}},
	}
	for name, o := range cases {
		_, h, _ := setup([]Rule{{Path: "/", Mode: ModeForce, TTL: 60}}, o)
		get(h, "/x")
		get(h, "/x")
		if o.calls.Load() != 2 {
			t.Errorf("%s was cached under force", name)
		}
	}
	o := &origin{}
	_, h, _ := setup([]Rule{{Path: "/", Mode: ModeForce, TTL: 60}}, o)
	get(h, "/x", "Authorization: Bearer a")
	get(h, "/x", "Authorization: Bearer b")
	if o.calls.Load() != 2 {
		t.Error("a request with Authorization was answered from the cache")
	}
}

func TestRulesBySegmentAndNever(t *testing.T) {
	o := &origin{}
	_, h, _ := setup([]Rule{{Path: "/product", Mode: ModeForce, TTL: 60}, {Path: "/product/admin", Mode: ModeNever}}, o)
	get(h, "/product/1")
	get(h, "/product/1")
	get(h, "/products-cheap")
	get(h, "/products-cheap")
	get(h, "/product/admin/x")
	get(h, "/product/admin/x")
	if o.calls.Load() != 5 {
		t.Fatalf("calls %d, want 5: /product/1 once, the rest twice", o.calls.Load())
	}
}

func TestVaryKeepsVariantsApart(t *testing.T) {
	o := &origin{header: map[string]string{"Vary": "Accept-Encoding"}}
	_, h, _ := setup([]Rule{{Path: "/", Mode: ModeForce, TTL: 60}}, o)
	gz := get(h, "/x", "Accept-Encoding: gzip")
	br := get(h, "/x", "Accept-Encoding: br")
	gz2 := get(h, "/x", "Accept-Encoding: gzip")
	if gz.Body.String() == br.Body.String() || gz2.Body.String() != gz.Body.String() || o.calls.Load() != 2 {
		t.Fatalf("calls %d: %q %q %q", o.calls.Load(), gz.Body, br.Body, gz2.Body)
	}
}

func TestFortySimultaneousMissesReachTheSiteOnce(t *testing.T) {
	o := &origin{header: map[string]string{"Cache-Control": "s-maxage=60"}, delay: 100 * time.Millisecond}
	_, h, _ := setup([]Rule{{Path: "/", Mode: ModeOrigin}}, o)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); get(h, "/expensive") }()
	}
	wg.Wait()
	if n := o.calls.Load(); n != 1 {
		t.Fatalf("%d calls to the site, want 1", n)
	}
}

func TestAHandoverEmptiesTheSite(t *testing.T) {
	o := &origin{header: map[string]string{"Cache-Control": "s-maxage=60"}}
	_, h, site := setup([]Rule{{Path: "/", Mode: ModeOrigin}}, o)
	get(h, "/x")
	site.Generation = "g2"
	if r := get(h, "/x"); r.Header().Get("X-Middleware-Cache") != "miss" {
		t.Fatal("the old instance's answer was served after a handover")
	}
}

func TestTheBudgetIsTakenFromTheLargestSite(t *testing.T) {
	c := New(4000)
	big := &origin{}
	small := &origin{}
	hb := c.Wrap(big, func(*http.Request) (Site, bool) {
		return Site{Domain: "big.example", Rules: []Rule{{Path: "/", Mode: ModeForce, TTL: 60}}}, true
	})
	hs := c.Wrap(small, func(*http.Request) (Site, bool) {
		return Site{Domain: "small.example", Rules: []Rule{{Path: "/", Mode: ModeForce, TTL: 60}}}, true
	})
	for i := 0; i < 3; i++ {
		get(hs, fmt.Sprintf("/s%d", i))
	}
	before := c.Stats("small.example").Bytes
	for i := 0; i < 200; i++ {
		get(hb, fmt.Sprintf("/b%d", i))
	}
	total, budget, _ := c.Total()
	if total > budget {
		t.Fatalf("%d bytes over a budget of %d", total, budget)
	}
	if c.Stats("small.example").Bytes != before {
		t.Fatalf("the small site paid for the big one: %d -> %d", before, c.Stats("small.example").Bytes)
	}
	_ = io.Discard
}
