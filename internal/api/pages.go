package api

import (
	"net/http"

	"github.com/dcimorra/the-middleware/internal/pages"
)

// registerPages: the designs of the visitor pages, and a preview of one,
// so the operator chooses by seeing and not by reading a name.
func (s *Server) registerPages() {
	s.Handle("GET", Prefix+"/pages/themes", "The designs of the visitor pages (key, name, note) and the house's", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{"themes": pages.Themes(), "house": pages.HouseTheme()}, nil
	})
	s.Handle("GET", Prefix+"/pages/preview", "A visitor page rendered in a design, as HTML (?theme=&page=down|starting|stopped|hold|forbidden|password)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		theme := r.URL.Query().Get("theme")
		if !pages.IsTheme(theme) {
			return nil, Errorf(http.StatusBadRequest, "%q is not one of the designs", theme)
		}
		key := r.URL.Query().Get("page")
		if key == "" {
			key = "down"
		}
		n := pages.ForSite(key, "example.com", "example.com", theme)
		// Its own policy: the page carries its own styles, and it is shown
		// in a frame of the panel, which nothing else may frame.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; frame-ancestors 'self'")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(pages.HTML(n))
		return Handled, nil
	})
}
