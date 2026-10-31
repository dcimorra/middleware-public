package httpcache

import (
	"net/http"
	"strings"
	"testing"
)

// Each service of a site keeps its own store, with its own generation: one
// shared, a request to the other service emptied it every time. A purge
// of the site empties all of them and its numbers are their sum.
func TestEachServiceHasItsOwnStoreAndThePurgeIsTheSites(t *testing.T) {
	o := &origin{header: map[string]string{"Cache-Control": "public, max-age=60"}}
	c := New(64 << 20)
	rules := []Rule{{Path: "/", Mode: ModeOrigin}}
	h := c.Wrap(o, func(r *http.Request) (Site, bool) {
		if strings.HasPrefix(r.URL.Path, "/api") {
			return Site{Domain: "a.example#api", Rules: rules, Generation: "api-1"}, true
		}
		return Site{Domain: "a.example#web", Rules: rules, Generation: "web-1"}, true
	})
	for i := 0; i < 2; i++ {
		get(h, "/page")
		get(h, "/api/x")
	}
	if n := o.calls.Load(); n != 2 {
		t.Fatalf("the origin was asked %d times, want once per service", n)
	}
	if st := c.Stats("a.example"); st.Entries != 2 || st.Hits != 2 {
		t.Fatalf("the site's numbers %+v", st)
	}
	c.Purge("a.example")
	if st := c.Stats("a.example"); st.Entries != 0 {
		t.Fatalf("after the purge %+v", st)
	}
}
