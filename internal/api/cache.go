package api

import (
	"net/http"

	"github.com/dcimorra/the-middleware/internal/httpcache"
)

// RegisterCache adds the response cache's routes.
func (s *Server) RegisterCache(c *httpcache.Cache) {
	s.Handle("GET", Prefix+"/cache", "The house's response cache: bytes kept, the budget, entries", func(w http.ResponseWriter, r *http.Request) (any, error) {
		b, budget, n := c.Total()
		return map[string]any{"bytes": b, "budget": budget, "entries": n}, nil
	})
	s.Handle("GET", Prefix+"/sites/{domain}/cache", "A site's response cache: entries, bytes, hits, misses", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		return c.Stats(domain), nil
	})
	s.Handle("DELETE", Prefix+"/sites/{domain}/cache", "Empty a site's response cache", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		c.Purge(domain)
		return OK, nil
	})
}
