package api

import (
	"errors"
	"net/http"

	"github.com/dcimorra/the-middleware/internal/repos"
)

// RegisterRepos adds the /repos routes. m may be nil; they answer 503.
func (s *Server) RegisterRepos(m *repos.Manager) {
	p := Prefix + "/repos"
	need := func() (*repos.Manager, error) {
		if m == nil {
			return nil, Errorf(http.StatusServiceUnavailable, "the repositories are not watched")
		}
		return m, nil
	}
	notFound := func(err error) error {
		if errors.Is(err, repos.ErrNotFound) {
			return Errorf(http.StatusNotFound, "%v", err)
		}
		return err
	}
	s.Handle("GET", p, "The git repositories inside the sites", func(w http.ResponseWriter, r *http.Request) (any, error) {
		rm, err := need()
		if err != nil {
			return nil, err
		}
		return rm.List(r.Context()), nil
	})
	s.Handle("GET", p+"/{id}/log", "A repository's last 30 commits", func(w http.ResponseWriter, r *http.Request) (any, error) {
		rm, err := need()
		if err != nil {
			return nil, err
		}
		c, err := rm.Log(r.Context(), r.PathValue("id"), 30)
		if c == nil && err == nil {
			c = []repos.Commit{}
		}
		return c, notFound(err)
	})
	s.Handle("POST", p+"/{id}/pull", "Fetch and merge the upstream now", func(w http.ResponseWriter, r *http.Request) (any, error) {
		rm, err := need()
		if err != nil {
			return nil, err
		}
		repo, err := rm.Pull(r.Context(), r.PathValue("id"))
		return repo, notFound(err)
	})
	s.Handle("PUT", p+"/{id}/auto", "Keep a repository up to date, or stop", func(w http.ResponseWriter, r *http.Request) (any, error) {
		rm, err := need()
		if err != nil {
			return nil, err
		}
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		repo, err := rm.SetAuto(r.Context(), r.PathValue("id"), body.Enabled)
		return repo, notFound(err)
	})
}
