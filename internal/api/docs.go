package api

import (
	"net/http"

	"github.com/dcimorra/the-middleware/docs"
)

func (s *Server) registerDocs() {
	s.Handle("GET", Prefix+"/docs", "The documents embedded in this binary", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return docs.List(), nil
	})
	s.Handle("GET", Prefix+"/docs/{name}", "One document as Markdown", func(w http.ResponseWriter, r *http.Request) (any, error) {
		data, ok := docs.Read(r.PathValue("name"))
		if !ok {
			return nil, Errorf(http.StatusNotFound, "no document %q", r.PathValue("name"))
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write(data)
		return Handled, nil
	})
}
