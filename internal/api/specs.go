package api

import (
	"context"
	"net/http"

	"github.com/dcimorra/middleware-public/internal/specs"
)

// SpecsOptions wire the Specs tab: a site's openspec/ folder read for the
// panel, and set up on request.
type SpecsOptions struct {
	// Enabled says whether the OpenSpec module is on: without it the tab
	// still reads what a project has, but cannot set one up.
	Enabled func() bool
	// Init runs `openspec init` in the site's folder.
	Init func(ctx context.Context, domain string) (string, error)
}

// RegisterSpecs mounts /sites/{domain}/specs.
func (s *Server) RegisterSpecs(o SpecsOptions) {
	enabled := func() bool { return o.Enabled != nil && o.Enabled() }
	site := func(r *http.Request) (string, string, error) {
		m, err := s.sitesManager()
		if err != nil {
			return "", "", err
		}
		domain, err := domainOf(r)
		if err != nil {
			return "", "", err
		}
		if _, err := m.Get(domain); err != nil {
			return "", "", Errorf(http.StatusNotFound, "%s: no such site", domain)
		}
		return domain, m.Dir(domain), nil
	}
	s.Handle("GET", Prefix+"/sites/{domain}/specs", "The site's OpenSpec folder: changes proposed with their tasks done, changes archived, and the specs of how it behaves today", func(w http.ResponseWriter, r *http.Request) (any, error) {
		_, dir, err := site(r)
		if err != nil {
			return nil, err
		}
		o2, err := specs.Read(dir)
		if err != nil {
			return nil, err
		}
		return map[string]any{"enabled": enabled(), "initialized": o2.Initialized, "changes": o2.Changes, "archived": o2.Archived, "specs": o2.Specs}, nil
	})
	s.Handle("POST", Prefix+"/sites/{domain}/specs/init", "Set the site's project up for OpenSpec (openspec init): the openspec/ folder and OpenSpec's skills for the agents", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, _, err := site(r)
		if err != nil {
			return nil, err
		}
		if !enabled() || o.Init == nil {
			return nil, Errorf(http.StatusConflict, "the OpenSpec module is off: enable it under Modules first")
		}
		out, err := o.Init(r.Context(), domain)
		if err != nil {
			return nil, Errorf(http.StatusBadGateway, "%v: %s", err, out)
		}
		return map[string]any{"ok": true, "output": out}, nil
	})
}
