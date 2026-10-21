package api

import (
	"net/http"
	"strings"

	"github.com/dcimorra/middleware-public/internal/proxy"
)

// RegisterUnrouted mounts the record of names nobody serves.
func (s *Server) RegisterUnrouted(u *proxy.Unrouted) {
	s.Handle("GET", Prefix+"/unrouted", "Names that reached this house and belong to no site, most recent first, with the door they came by and the last calls", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return u.List(), nil
	})
	s.Handle("DELETE", Prefix+"/unrouted/{host}", "Forget a name in that record", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if !u.Forget(strings.ToLower(r.PathValue("host"))) {
			return nil, Errorf(http.StatusNotFound, "not in the record")
		}
		return OK, nil
	})
}
