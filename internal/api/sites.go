package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/sites"
)

func (s *Server) registerSites() {
	p := Prefix + "/sites"
	s.Handle("GET", p, "List every site folder, unmanaged ones included", s.sitesList)
	s.Handle("POST", p, "Create a site from a manifest or a template (static, node, go, docker)", s.sitesCreate)
	s.Handle("GET", p+"/{domain}", "One site", s.sitesGet)
	s.Handle("PUT", p+"/{domain}/manifest", "Replace the manifest", s.sitesPutManifest)
	s.Handle("PATCH", p+"/{domain}/manifest", "Merge top-level keys into the manifest; null removes a key", s.sitesPatchManifest)
	s.Handle("DELETE", p+"/{domain}", "Delete the folder, or move it to data/archive/ with ?archive=true", s.sitesDelete)
	s.Handle("POST", p+"/{domain}/restart", "Force a handover", s.sitesRestart)
	s.Handle("POST", p+"/{domain}/stop", "Stop the site (disabled: true)", s.sitesStop)
	s.Handle("POST", p+"/{domain}/start", "Start the site (clears disabled)", s.sitesStart)
	s.Handle("POST", p+"/{domain}/hold", "Put the site behind the holding page, or take it out", s.sitesHold)
	s.Handle("GET", p+"/{domain}/logs", "The site's or a worker's log as text; ?follow=1 streams", s.sitesLogs)
	s.Handle("GET", p+"/{domain}/files", "List a folder inside the site", s.sitesFiles)
	s.Handle("GET", p+"/{domain}/file", "Read a file inside the site", s.sitesReadFile)
	s.Handle("PUT", p+"/{domain}/file", "Write a file inside the site (raw body)", s.sitesWriteFile)
	s.Handle("POST", p+"/{domain}/deploy", "Run deploy.github.build and write the pointer", s.sitesDeploy)
	s.Handle("GET", p+"/{domain}/env", "The environment the instance is born with, secrets masked", s.sitesEnv)
}

func (s *Server) sitesManager() (*sites.Manager, error) {
	if s.opts.Sites == nil {
		return nil, Errorf(http.StatusNotImplemented, "no sites manager")
	}
	return s.opts.Sites, nil
}

func (s *Server) sitesList(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	return m.List()
}

func (s *Server) sitesGet(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	return m.Get(domain)
}

func (s *Server) sitesCreate(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	var body struct {
		Domain   string             `json:"domain"`
		Manifest *manifest.Manifest `json:"manifest"`
		Template string             `json:"template"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	site, err := m.Create(strings.ToLower(strings.TrimSpace(body.Domain)), body.Manifest, body.Template)
	if err != nil {
		return nil, err
	}
	WriteJSON(w, http.StatusCreated, site)
	return Handled, nil
}

func (s *Server) sitesPutManifest(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	var mf manifest.Manifest
	if err := ReadJSON(r, &mf); err != nil {
		return nil, err
	}
	return m.Update(domain, &mf)
}

func (s *Server) sitesPatchManifest(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	var partial map[string]json.RawMessage
	if err := ReadJSON(r, &partial); err != nil {
		return nil, err
	}
	return m.Patch(domain, partial)
}

func (s *Server) sitesDelete(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	archive := isTrue(r.URL.Query().Get("archive"))
	dest, err := m.Delete(r.Context(), domain, archive)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"ok": true, "archived": archive}
	if dest != "" {
		out["path"] = dest
	}
	return out, nil
}

func (s *Server) sitesRestart(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	if err := m.Restart(r.Context(), domain); err != nil {
		return nil, Errorf(http.StatusConflict, "%v", err)
	}
	return OK, nil
}

func (s *Server) sitesStop(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.setDisabled(r, true)
}

func (s *Server) sitesStart(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.setDisabled(r, false)
}

func (s *Server) setDisabled(r *http.Request, disabled bool) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	return m.SetDisabled(domain, disabled)
}

func (s *Server) sitesHold(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	var hold manifest.Hold
	if err := ReadJSON(r, &hold); err != nil {
		return nil, err
	}
	return m.SetHold(domain, hold)
}

func (s *Server) sitesLogs(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	lines := 200
	if v := q.Get("lines"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100000 {
			return nil, Errorf(http.StatusBadRequest, "lines: want 0..100000")
		}
		lines = n
	}
	worker := q.Get("worker")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !isTrue(q.Get("follow")) {
		out, err := m.Logs(domain, worker, lines)
		if err != nil {
			return nil, err
		}
		_, _ = io.WriteString(w, out)
		return Handled, nil
	}
	if _, err := m.LogPath(domain, worker); err != nil {
		return nil, err
	}
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	_ = m.Follow(r.Context(), domain, worker, lines, w)
	return Handled, nil
}

func (s *Server) sitesFiles(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	return m.Files(domain, r.URL.Query().Get("path"))
}

func (s *Server) sitesReadFile(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	p := r.URL.Query().Get("path")
	if p == "" {
		return nil, Errorf(http.StatusBadRequest, "path: required")
	}
	data, err := m.ReadFile(domain, p)
	if err != nil {
		return nil, err
	}
	// Always a download, never rendered: a site's HTML served from the
	// panel host would run with the panel's cookies.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
	return Handled, nil
}

func (s *Server) sitesWriteFile(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	p := r.URL.Query().Get("path")
	if p == "" {
		return nil, Errorf(http.StatusBadRequest, "path: required")
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, sites.MaxFileSize+1))
	if err != nil {
		return nil, Errorf(http.StatusRequestEntityTooLarge, "body: %v", err)
	}
	if err := m.WriteFile(domain, p, data); err != nil {
		return nil, err
	}
	return OK, nil
}

func (s *Server) sitesDeploy(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	// A deployer set by the wiring (the github module's runner, which
	// fetches the repository first) takes the request when it wants it;
	// otherwise the manager builds what is already in the folder.
	s.mu.Lock()
	deployer := s.deployer
	s.mu.Unlock()
	if deployer != nil {
		if out, handled, err := deployer(domain); handled {
			return out, err
		}
	}
	id, err := m.Deploy(domain)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "build": id}, nil
}

func (s *Server) sitesEnv(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	env, err := m.ResolvedEnv(domain)
	if err != nil {
		return nil, err
	}
	return map[string]any{"resolved": env}, nil
}

func isTrue(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
