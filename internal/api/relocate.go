package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/inbound"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/nodes"
	"github.com/dcimorra/middleware-public/internal/relocate"
	"github.com/dcimorra/middleware-public/internal/sites"
)

// Moving a site to another node of the network. Both ends are here: the
// routes the moving node calls on the target (receive, the last copy,
// commit, discard), and the operator's POST /sites/{domain}/move on the
// source, which runs the move in the background and says each step as an
// event (relocate.*) and in GET /sites/{domain}/move.

// moveJob is one move in flight or finished.
type moveJob struct {
	Node    string           `json:"node"`
	Step    string           `json:"step"`
	Message string           `json:"message"`
	Error   string           `json:"error,omitempty"`
	Done    bool             `json:"done"`
	Started time.Time        `json:"started"`
	Report  *relocate.Report `json:"report,omitempty"`
}

type moves struct {
	mu   sync.Mutex
	jobs map[string]*moveJob
}

// registerRelocateTarget mounts the receiving end. It needs no mesh: the
// routes are what another node calls with the network's secret, and a
// node may call anything but the network's own routes.
func (s *Server) registerRelocateTarget() {
	rc := relocate.Receiver{Root: s.opts.Root}
	bad := func(err error) error {
		switch {
		case errors.Is(err, relocate.ErrExists):
			return Errorf(http.StatusConflict, "%v", err)
		case errors.Is(err, relocate.ErrBadName), errors.Is(err, relocate.ErrNoStaging):
			return Errorf(http.StatusBadRequest, "%v", err)
		}
		return err
	}
	s.Handle("POST", Prefix+"/relocate/{domain}/check", "Whether this node can take a site moving in (body: names, the ones published where it is): the name is free here and every name can be published by this node", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain := r.PathValue("domain")
		if !manifest.ValidName(domain) {
			return nil, Errorf(http.StatusBadRequest, "%v", relocate.ErrBadName)
		}
		mgr, err := s.sitesManager()
		if err != nil {
			return nil, err
		}
		if _, err := mgr.Get(domain); err == nil {
			return nil, Errorf(http.StatusConflict, "%s is already a site on this node", domain)
		}
		var body struct {
			Names []string `json:"names"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		if len(body.Names) == 0 {
			return OK, nil
		}
		if s.publisher == nil {
			return nil, Errorf(http.StatusConflict, "this node publishes nothing: its names would not follow")
		}
		_, no, err := s.publisher.Covers(r.Context(), body.Names)
		if err != nil {
			return nil, Errorf(http.StatusBadGateway, "%v", err)
		}
		if len(no) > 0 {
			return nil, Errorf(http.StatusConflict, "this node cannot publish %s (no DNS provider for their zone here): its names would not follow; enable the cloudflare module here first", strings.Join(no, ", "))
		}
		return OK, nil
	})
	s.Handle("PUT", Prefix+"/relocate/{domain}", "Receive a site moving in from another node: its bundle (.tar.gz) as the raw body, kept apart until commit", func(w http.ResponseWriter, r *http.Request) (any, error) {
		rep, err := rc.Receive(r.Context(), r.PathValue("domain"), r.Body)
		if err != nil {
			return nil, bad(err)
		}
		return rep, nil
	})
	s.Handle("PUT", Prefix+"/relocate/{domain}/delta", "The last copy of a site moving in: what changed since the first, and the index of what is there", func(w http.ResponseWriter, r *http.Request) (any, error) {
		rep, err := rc.ReceiveDelta(r.Context(), r.PathValue("domain"), r.Body)
		if err != nil {
			return nil, bad(err)
		}
		return rep, nil
	})
	s.Handle("POST", Prefix+"/relocate/{domain}/commit", "Put a received site in place (body: manifest, base64: the one it had before it stopped); the engine starts it", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var body struct {
			Manifest string `json:"manifest"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		raw, err := base64.StdEncoding.DecodeString(body.Manifest)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "manifest: not base64")
		}
		domain := r.PathValue("domain")
		if err := rc.Commit(domain, raw); err != nil {
			return nil, bad(err)
		}
		s.emitMove("relocate.arrived", domain, "", "arrived from another node", nil)
		return OK, nil
	})
	s.Handle("DELETE", Prefix+"/relocate/{domain}", "Throw away what was received for a site that did not finish moving in", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := rc.Discard(r.PathValue("domain")); err != nil {
			return nil, bad(err)
		}
		return OK, nil
	})
}

// registerRelocateSource mounts the operator's move: it needs the mesh.
func (s *Server) registerRelocateSource(m *nodes.Mesh) {
	s.Handle("POST", Prefix+"/sites/{domain}/move", "Move the site to another node of the network (body: node): copied while it serves, stopped for the last copy, started there, names pointed there, archived here", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		var body struct {
			Node string `json:"node"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		x, ok := m.Member(body.Node)
		if !ok {
			return nil, Errorf(http.StatusNotFound, "%v", nodes.ErrNotFound)
		}
		if x.ID == m.Self().ID {
			return nil, Errorf(http.StatusBadRequest, "%s is this node", x.Name)
		}
		mgr, err := s.sitesManager()
		if err != nil {
			return nil, err
		}
		if _, err := mgr.Get(domain); err != nil {
			return nil, err
		}
		// Asked now, not only when the move starts: a target that cannot
		// take it is said in the answer, not in an event a minute later.
		src := siteSource{s: s, mgr: mgr}
		var names []string
		if _, ok := src.Published(domain); ok {
			names = src.Names(domain)
		}
		cctx, ccancel := context.WithTimeout(r.Context(), 30*time.Second)
		err = (meshTarget{m: m, x: x}).Check(cctx, domain, names)
		ccancel()
		if err != nil {
			return nil, Errorf(http.StatusConflict, "%s cannot take %s: %v", x.Name, domain, err)
		}
		s.moves.mu.Lock()
		if s.moves.jobs == nil {
			s.moves.jobs = map[string]*moveJob{}
		}
		if j := s.moves.jobs[domain]; j != nil && !j.Done {
			s.moves.mu.Unlock()
			return nil, Errorf(http.StatusConflict, "%s is already moving to %s (%s)", domain, j.Node, j.Step)
		}
		job := &moveJob{Node: x.Name, Step: relocate.StepCopy, Started: time.Now()}
		s.moves.jobs[domain] = job
		s.moves.mu.Unlock()
		mover := &relocate.Mover{Root: s.opts.Root, Source: src}
		go func() {
			// A move outlives the request that asked for it: an hour for a
			// big site over a slow link.
			ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
			defer cancel()
			rep, err := mover.Move(ctx, domain, meshTarget{m: m, x: x}, func(step, msg string) {
				s.moves.mu.Lock()
				job.Step, job.Message = step, msg
				s.moves.mu.Unlock()
				s.emitMove("relocate."+step, domain, "", msg, map[string]any{"node": x.Name})
			})
			s.moves.mu.Lock()
			job.Done, job.Report = true, &rep
			if err != nil {
				job.Error = err.Error()
			}
			s.moves.mu.Unlock()
			if err != nil {
				s.emitMove("relocate.failed", domain, "error", err.Error(), map[string]any{"node": x.Name, "step": job.Step})
			}
		}()
		return job, nil
	})
	s.Handle("GET", Prefix+"/sites/{domain}/move", "The site's move to another node: its step, or how it ended", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		s.moves.mu.Lock()
		defer s.moves.mu.Unlock()
		if j := s.moves.jobs[domain]; j != nil {
			c := *j
			return c, nil
		}
		return nil, Errorf(http.StatusNotFound, "%s has not been moved since this node started", domain)
	})
}

func (s *Server) emitMove(typ, domain, level, msg string, data map[string]any) {
	if s.opts.Bus == nil {
		return
	}
	s.opts.Bus.Emit(events.Event{Type: typ, Subject: domain, Actor: "engine", Level: level, Message: msg, Data: data})
}

// siteSource is this node's side of a move.
type siteSource struct {
	s   *Server
	mgr *sites.Manager
}

func (src siteSource) Stop(ctx context.Context, domain string) ([]byte, error) {
	return src.mgr.StopForMove(ctx, domain)
}

func (src siteSource) Start(domain string, m []byte) error { return src.mgr.Resume(domain, m) }

func (src siteSource) Published(domain string) (string, bool) {
	if src.s.publisher == nil {
		return "", false
	}
	door, ok := src.s.publisher.Published(domain)
	return string(door), ok
}

func (src siteSource) Names(domain string) []string {
	if src.s.publisher == nil {
		return nil
	}
	return src.s.publisher.HostsOf(domain)
}

func (src siteSource) Unpublish(ctx context.Context, domain string) {
	if src.s.publisher != nil {
		src.s.publisher.Unpublish(ctx, domain)
	}
}

func (src siteSource) Archive(ctx context.Context, domain string, m []byte) (string, error) {
	return src.mgr.ArchiveMoved(ctx, domain, m)
}

// meshTarget is a node of the network as a move's target.
type meshTarget struct {
	m *nodes.Mesh
	x nodes.Member
}

func (t meshTarget) Name() string { return t.x.Name }

// older says a node that answers 404 to a move's route runs a version
// without moves, which is what to tell the operator.
func (t meshTarget) older(err error) error {
	var re *nodes.RemoteError
	if errors.As(err, &re) && re.Status == http.StatusNotFound && strings.Contains(re.Message, "no such route") {
		return fmt.Errorf("%s runs a version that cannot receive sites: update it first", t.x.Name)
	}
	return err
}

func (t meshTarget) Send(ctx context.Context, domain string, body io.Reader) error {
	return t.older(t.m.Stream(ctx, t.x, "PUT", "/relocate/"+domain, "application/gzip", body, nil))
}

func (t meshTarget) SendDelta(ctx context.Context, domain string, body io.Reader) error {
	return t.older(t.m.Stream(ctx, t.x, "PUT", "/relocate/"+domain+"/delta", "application/gzip", body, nil))
}

func (t meshTarget) Commit(ctx context.Context, domain string, m []byte) error {
	return t.m.Call(ctx, t.x, "POST", "/relocate/"+domain+"/commit", map[string]string{"manifest": base64.StdEncoding.EncodeToString(m)}, nil)
}

func (t meshTarget) Discard(ctx context.Context, domain string) error {
	return t.m.Call(ctx, t.x, "DELETE", "/relocate/"+domain, nil, nil)
}

func (t meshTarget) Remove(ctx context.Context, domain string) error {
	return t.m.Call(ctx, t.x, "DELETE", "/sites/"+domain, nil, nil)
}

func (t meshTarget) Check(ctx context.Context, domain string, names []string) error {
	return t.older(t.m.Call(ctx, t.x, "POST", "/relocate/"+domain+"/check", map[string]any{"names": names}, nil))
}

func (t meshTarget) Ready(ctx context.Context, domain string) (bool, string, error) {
	var site sites.Site
	if err := t.m.Call(ctx, t.x, "GET", "/sites/"+domain, nil, &site); err != nil {
		return false, "", err
	}
	ok, why := SiteReady(site)
	return ok, why, nil
}

func (t meshTarget) Publish(ctx context.Context, domain, door string) error {
	return t.m.Call(ctx, t.x, "POST", "/sites/"+domain+"/publish", map[string]any{"door": inbound.Door(door), "over": true}, nil)
}

// SiteReady says a site answers as it should: no error, and its process
// alive, or its workers for a project, or nothing to run for a site that
// serves files.
func SiteReady(s sites.Site) (bool, string) {
	if s.Error != "" {
		return false, s.Error
	}
	if s.Alive {
		return true, ""
	}
	switch s.Mode {
	case manifest.ModeStatic:
		return true, ""
	case manifest.ModeProject:
		for name, w := range s.Workers {
			if !w.Alive {
				return false, "worker " + name + " is not up"
			}
		}
		return len(s.Workers) > 0, "no worker is up yet"
	}
	return false, "not up yet"
}
