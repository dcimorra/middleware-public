package api

import (
	"net"
	"net/http"

	"github.com/dcimorra/middleware-public/internal/security"
)

// SecurityOptions wire the security door's routes. Door may be nil if
// the hook did not start; the routes then answer 503.
type SecurityOptions struct {
	Door    *security.Door
	Country func(r *http.Request, ip net.IP) string
}

// RegisterSecurity adds the routes the panel's security screen reads.
// The rules themselves are the manifest's `security` and are written
// through the manifest, like every other decision about a site.
func (s *Server) RegisterSecurity(o SecurityOptions) {
	s.Handle("GET", Prefix+"/security/catalog", "Robot families and their robots, browsers and the paths every site refuses", func(w http.ResponseWriter, r *http.Request) (any, error) {
		country := ""
		if o.Country != nil {
			country = o.Country(r, nil)
		}
		return map[string]any{
			"families":      security.Families(),
			"robots":        security.Robots(),
			"browsers":      security.Browsers(),
			"builtin_paths": security.BuiltinPaths(),
			// The country of the request this is asked with: an allow list
			// of countries locks everyone out if the edge sends none.
			"your_country": country,
		}, nil
	})
	s.Handle("GET", Prefix+"/sites/{domain}/security/blocks", "The last 50 requests the security door refused, newest first", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if o.Door == nil {
			return nil, Errorf(http.StatusServiceUnavailable, "the security door did not start")
		}
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		return o.Door.Recent(domain), nil
	})
}
