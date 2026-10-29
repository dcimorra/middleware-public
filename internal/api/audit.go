package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/dcimorra/the-middleware/internal/audit"
)

// RegisterAudit adds the dependency audit's routes. a may be nil: 503.
// site resolves a domain to its folder.
func (s *Server) RegisterAudit(a *audit.Auditor, site func(domain string) (audit.Site, bool)) {
	need := func() (*audit.Auditor, error) {
		if a == nil {
			return nil, Errorf(http.StatusServiceUnavailable, "the dependency audit is not running")
		}
		return a, nil
	}
	s.Handle("GET", Prefix+"/audit", "Every site's last dependency audit", func(w http.ResponseWriter, r *http.Request) (any, error) {
		au, err := need()
		if err != nil {
			return nil, err
		}
		out := au.Reports()
		if out == nil {
			out = []audit.Report{}
		}
		return out, nil
	})
	s.Handle("POST", Prefix+"/audit/run", "Audit every site now, in the background (deps.audited when done)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		au, err := need()
		if err != nil {
			return nil, err
		}
		go au.CheckAll(context.Background())
		return OK, nil
	})
	s.Handle("POST", Prefix+"/sites/{domain}/audit", "Audit one site now and wait for it", func(w http.ResponseWriter, r *http.Request) (any, error) {
		au, err := need()
		if err != nil {
			return nil, err
		}
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		st, ok := site(domain)
		if !ok {
			return nil, Errorf(http.StatusNotFound, "no such site")
		}
		return au.Check(r.Context(), st), nil
	})
	// Publishing a site with known critical vulnerabilities says so: it
	// is the moment a site starts being reachable, and the last one at
	// which "it was already like that" is still news.
	s.vulnerable = func(domain string) string {
		if a == nil {
			return ""
		}
		rep, ok := a.Report(domain)
		if !ok || len(rep.Critical) == 0 {
			return ""
		}
		var names []string
		for _, c := range rep.Critical {
			names = append(names, c.Name+" "+c.Range)
		}
		return fmt.Sprintf("%s has %d critical vulnerabilities in its dependencies (audited %s): %s", domain, len(rep.Critical), rep.At.Format("2 Jan 15:04"), strings.Join(names, ", "))
	}
}
