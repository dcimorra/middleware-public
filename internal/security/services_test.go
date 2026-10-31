package security

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAServicesRulesAreJudgedAfterTheSitesNeverInsteadOfThem(t *testing.T) {
	site := &Rules{Agents: &List{Deny: []string{"badbot"}}}
	api := &Rules{Agents: &List{Deny: []string{"curl"}}, Paths: []string{"/api/internal"}}
	d := New(Options{
		LookupRequest: func(r *http.Request) (Site, bool) {
			s := Site{Domain: "shop.example", Rules: site}
			if strings.HasPrefix(r.URL.Path, "/api") {
				s.More, s.Service = api, "api"
			}
			return s, true
		},
	})
	h := d.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	for _, c := range []struct {
		path, ua string
		pass     bool
	}{
		{"/", "curl/8", true},
		{"/api/x", "curl/8", false},
		{"/api/x", "badbot/1", false},
		{"/api/x", "Mozilla/5.0", true},
		{"/api/internal/keys", "Mozilla/5.0", false},
		{"/.env", "Mozilla/5.0", false},
	} {
		r := httptest.NewRequest("GET", "http://shop.example"+c.path, nil)
		r.Header.Set("User-Agent", c.ua)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if (rec.Code == 200) != c.pass {
			t.Errorf("%s as %q: %d, pass %v", c.path, c.ua, rec.Code, c.pass)
		}
	}
	if len(d.Recent("shop.example")) != 4 {
		t.Fatalf("refusals are the site's, kept under its name: %d", len(d.Recent("shop.example")))
	}
}
