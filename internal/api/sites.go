package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/archive"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/inbound"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/shots"
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
	s.Handle("POST", p+"/{domain}/rename", "Rename a site (body: to): folder, HOME, logs, conversations and cost move with it; published names follow", s.sitesRename)
	s.Handle("GET", p+"/{domain}/download.zip", "The site folder as a zip, without what an install or a build gives back (?all=1 for everything)", s.sitesDownload)
	s.Handle("GET", Prefix+"/archive", "Archived sites, newest first", func(w http.ResponseWriter, r *http.Request) (any, error) {
		m, err := s.sitesManager()
		if err != nil {
			return nil, err
		}
		list := m.Archives()
		if list == nil {
			list = []sites.Archived{}
		}
		return list, nil
	})
	s.Handle("POST", Prefix+"/archive/{name}/restore", "Put an archived site back in sites/", func(w http.ResponseWriter, r *http.Request) (any, error) {
		m, err := s.sitesManager()
		if err != nil {
			return nil, err
		}
		return m.Restore(context.Background(), r.PathValue("name"))
	})
	s.Handle("DELETE", Prefix+"/archive/{name}", "Delete an archive for good", func(w http.ResponseWriter, r *http.Request) (any, error) {
		m, err := s.sitesManager()
		if err != nil {
			return nil, err
		}
		if err := m.DeleteArchive(r.PathValue("name")); err != nil {
			return nil, err
		}
		return OK, nil
	})
	s.Handle("GET", Prefix+"/archive/{name}/download", "Download an archive (.tar.gz)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		m, err := s.sitesManager()
		if err != nil {
			return nil, err
		}
		path, _, err := m.ArchivePath(r.PathValue("name"))
		if err != nil {
			return nil, err
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+r.PathValue("name")+`"`)
		http.ServeFile(w, r, path)
		return Handled, nil
	})
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
		// Publish defaults to true when a DNS provider is enabled: what
		// "create a site" means is a site people can reach. Door is the
		// door to publish on; empty is the door for new sites.
		Publish *bool        `json:"publish"`
		Door    inbound.Door `json:"door"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	domain := strings.ToLower(strings.TrimSpace(body.Domain))
	site, err := m.Create(domain, body.Manifest, body.Template)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	raw, _ := json.Marshal(site)
	_ = json.Unmarshal(raw, &out)
	want := s.dnsEnabled != nil && s.dnsEnabled()
	if body.Publish != nil {
		want = *body.Publish
	}
	if want && s.publisher != nil && !manifest.IsProject(domain) {
		// In the background: the engine has to see the folder first,
		// and a certificate for the ports door takes the CA's time. The
		// outcome is in GET /publish and in publish.* events.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			for i := 0; i < 20 && len(s.publisher.HostsOf(domain)) == 0; i++ {
				time.Sleep(500 * time.Millisecond)
			}
			if _, err := s.publisher.Publish(ctx, domain, body.Door); err != nil {
				s.opts.Bus.Emit(events.Event{Type: "publish.failed", Subject: domain, Actor: "operator", Level: "error", Message: err.Error()})
			}
		}()
		out["publishing"] = true
	}
	WriteJSON(w, http.StatusCreated, out)
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
	keep := isTrue(r.URL.Query().Get("archive"))
	// Off the internet first, while the engine still knows its names: a
	// deleted site whose record and tunnel rule stay behind answers
	// "nothing here" for as long as nobody notices. A name that cannot
	// be removed now is retried by the publisher, not forgotten.
	var unpublished []inbound.Line
	if s.publisher != nil {
		if _, ok := s.publisher.Published(domain); ok {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
			unpublished = s.publisher.Unpublish(ctx, domain)
			cancel()
		}
	}
	// Packing a large site takes minutes: it must not stop halfway because
	// the browser tab went away.
	dest, err := m.Delete(context.Background(), domain, keep)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"ok": true, "archived": keep}
	if unpublished != nil {
		out["unpublished"] = unpublished
	}
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

func (s *Server) sitesRename(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	from, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	var body struct {
		To string `json:"to"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	to := strings.ToLower(strings.TrimSpace(body.To))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// The old names leave the internet with the old site, and the new
	// ones are published on the same door: a rename that left DNS on the
	// old name would be half a rename.
	var door inbound.Door
	published := false
	if s.publisher != nil {
		if door, published = s.publisher.Published(from); published {
			s.publisher.Unpublish(ctx, from)
		}
	}
	site, err := m.Rename(ctx, from, to)
	if err != nil {
		return nil, err
	}
	if published {
		go func() {
			pctx, pcancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer pcancel()
			for i := 0; i < 20 && len(s.publisher.HostsOf(to)) == 0; i++ {
				time.Sleep(500 * time.Millisecond)
			}
			_, _ = s.publisher.Publish(pctx, to, door)
		}()
	}
	return site, nil
}

func (s *Server) sitesDownload(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	dir := m.Dir(domain)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, Errorf(http.StatusNotFound, "%s: no such site", domain)
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+domain+`.zip"`)
	// Streamed: a site can be gigabytes, and the zip is written as it is
	// read. An error halfway can only cut the stream, which the client
	// sees as a broken download rather than a wrong one.
	if _, err := archive.Zip(r.Context(), w, dir, isTrue(r.URL.Query().Get("all"))); err != nil {
		s.opts.Bus.Emit(events.Event{Type: "download.failed", Subject: domain, Actor: "operator", Level: "warn", Message: err.Error()})
	}
	return Handled, nil
}

// RegisterShots adds the site pictures.
func (s *Server) RegisterShots(sh *shots.Shotter) {
	p := Prefix + "/sites"
	s.Handle("GET", p+"/{domain}/shot.png", "The site's picture; asking for it keeps pictures renewing for an hour", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		sh.Viewed()
		path, at, ok := sh.Path(domain)
		if !ok || time.Since(at) > shots.Refresh {
			sh.Request(domain, false)
		}
		if !ok {
			return nil, Errorf(http.StatusNotFound, "no picture of %s yet", domain)
		}
		w.Header().Set("Cache-Control", "private, max-age=60")
		http.ServeFile(w, r, path)
		return Handled, nil
	})
	s.Handle("POST", p+"/{domain}/shot", "Take the site's picture now, whatever state it is in", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		if !sh.Available() {
			return nil, Errorf(http.StatusNotImplemented, "no headless Chromium on this machine (set MIDDLEWARE_CHROME)")
		}
		sh.Request(domain, true)
		return OK, nil
	})
}
