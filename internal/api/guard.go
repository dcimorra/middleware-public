package api

import (
	"net/http"
	"os"

	"github.com/dcimorra/middleware-public/internal/guard"
)

// GuardOptions wire the guard section.
type GuardOptions struct {
	Guard *guard.Guard
}

// RegisterGuard adds the /guard routes. The rules are listed from the
// code that runs them, so the panel can never show a rule the guard does
// not apply.
func (s *Server) RegisterGuard(o GuardOptions) {
	p := Prefix + "/guard"
	s.Handle("GET", p, "The rules and the incidents not yet dismissed", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if o.Guard == nil {
			return nil, Errorf(http.StatusServiceUnavailable, "the guard is not running")
		}
		all, err := o.Guard.Incidents()
		if err != nil {
			return nil, err
		}
		incidents := []guard.Incident{}
		for _, inc := range all {
			if !inc.Dismissed || r.URL.Query().Get("all") == "1" {
				incidents = append(incidents, inc)
			}
		}
		rules := o.Guard.Rules()
		if rules == nil {
			rules = []guard.Rule{}
		}
		return map[string]any{"rules": rules, "incidents": incidents}, nil
	})
	s.Handle("GET", Prefix+"/sites/{domain}/resources", "A site's CPU and memory, a sample a minute for a day, and its episodes", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if o.Guard == nil {
			return nil, Errorf(http.StatusServiceUnavailable, "the guard is not running")
		}
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		return o.Guard.Resources(domain), nil
	})
	s.Handle("POST", p+"/incidents/{id}/dismiss", "Mark an incident as seen", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if o.Guard == nil {
			return nil, Errorf(http.StatusServiceUnavailable, "the guard is not running")
		}
		if err := o.Guard.Dismiss(r.PathValue("id")); err != nil {
			if os.IsNotExist(err) {
				return nil, Errorf(http.StatusNotFound, "no incident %q", r.PathValue("id"))
			}
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		return OK, nil
	})
}
