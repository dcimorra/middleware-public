package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/dcimorra/middleware-public/internal/domains"
)

// DefaultTLDs are searched when the request names none.
var DefaultTLDs = []string{"com", "net", "org", "es", "eu", "io", "dev", "app", "co", "me"}

// RegisterDomains adds the domain search.
func (s *Server) RegisterDomains(d *domains.Searcher) {
	s.Handle("GET", Prefix+"/domains/prices", "The hand-written yearly prices by ending, and the day they were written", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return d.Prices(), nil
	})
	s.Handle("PUT", Prefix+"/domains/prices", "Replace the price table (body: usd: {ending: price}); it is dated today", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			USD map[string]float64 `json:"usd"`
		}
		if err := ReadJSON(r, &b); err != nil {
			return nil, err
		}
		p, err := d.SetPrices(b.USD)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		return p, nil
	})
	s.Handle("GET", Prefix+"/domains/search", "Whether a name is free under some endings (?name=&tlds=com,es), by RDAP, with the hand-written prices", func(w http.ResponseWriter, r *http.Request) (any, error) {
		tlds := DefaultTLDs
		if v := r.URL.Query().Get("tlds"); v != "" {
			tlds = strings.Split(v, ",")
		}
		if len(tlds) > 40 {
			return nil, Errorf(http.StatusBadRequest, "at most 40 endings at once")
		}
		a, err := d.Search(r.Context(), r.URL.Query().Get("name"), tlds)
		if errors.Is(err, domains.ErrBadName) {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		return a, err
	})
}
