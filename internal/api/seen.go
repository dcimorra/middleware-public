package api

import (
	"net/http"

	"github.com/dcimorra/middleware-public/internal/seen"
)

// RegisterSeen mounts a site's tally of callers.
func (s *Server) RegisterSeen(b *seen.Book) {
	s.Handle("GET", Prefix+"/sites/{domain}/security/seen", "Who called at the site: countries, robots, browsers and unknown User-Agents, heaviest first, with counts and first and last seen", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		return b.Table(domain), nil
	})
	s.Handle("POST", Prefix+"/sites/{domain}/security/seen/forget", "Empty the site's tally, so it says what calls now", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		b.Forget(domain)
		return OK, nil
	})
}
