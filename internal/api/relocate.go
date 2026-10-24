package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dcimorra/middleware-public/internal/auth"
	goruntime "github.com/dcimorra/middleware-public/internal/runtime"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
		var body relocate.Needs
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		if body.Docker {
			if err := dockerAnswers(r.Context()); err != nil {
				return nil, Errorf(http.StatusConflict, "this node has no working docker (%v) and the site runs in a container: install docker here (run middleware install again), or take the site out of its container before moving it", err)
			}
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
	s.Handle("POST", Prefix+"/relocate/{domain}/prepare", "Install a received site's dependencies in its staging, for this machine, before it is stopped where it is", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain := r.PathValue("domain")
		if _, err := os.Stat(rc.StagingSite(domain)); err != nil {
			return nil, bad(relocate.ErrNoStaging)
		}
		ictx, icancel := context.WithTimeout(r.Context(), 15*time.Minute)
		defer icancel()
		if err := installDeps(ictx, rc.StagingSite(domain)); err != nil {
			return nil, Errorf(http.StatusUnprocessableEntity, "%v", err)
		}
		return OK, nil
	})
	s.Handle("POST", Prefix+"/relocate/{domain}/commit", "Put a received site in place (body: manifest, base64: the one it had before it stopped); the engine starts it", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var body struct {
			Manifest string `json:"manifest"`
			// Install: the site's dependencies were not sent (an import
			// leaves node_modules behind, built for the other machine);
			// they are installed here before the site is put in place.
			Install bool `json:"install"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		raw, err := base64.StdEncoding.DecodeString(body.Manifest)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "manifest: not base64")
		}
		domain := r.PathValue("domain")
		if body.Install {
			ictx, icancel := context.WithTimeout(r.Context(), 15*time.Minute)
			err := installDeps(ictx, rc.StagingSite(domain))
			icancel()
			if err != nil {
				return nil, Errorf(http.StatusUnprocessableEntity, "installing its dependencies here failed, so it was not put in place: %v", err)
			}
		}
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

// registerImportToken mounts the operator's way to let a site in from a V1
// house: a token for that one site, two hours, opening only what an
// import calls, and the command to run on the V1 machine with it.
func (s *Server) registerImportToken() {
	s.Handle("POST", Prefix+"/relocate/import-token", "A token for importing one site from a V1 house (body: domain): it opens only that site's import routes, for two hours, and comes with the command to run on the V1 machine", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if c := auth.FromContext(r.Context()); len(c.Scopes) > 0 {
			return nil, Errorf(http.StatusForbidden, "only the operator makes import tokens")
		}
		var body struct {
			Domain string `json:"domain"`
			// Base is the address the V1 machine reaches this house at;
			// empty is the panel's own.
			Base string `json:"base"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		domain := strings.ToLower(strings.TrimSpace(body.Domain))
		if !manifest.ValidName(domain) {
			return nil, Errorf(http.StatusBadRequest, "%q is not a site's name", body.Domain)
		}
		if mgr, err := s.sitesManager(); err == nil {
			if _, err := mgr.Get(domain); err == nil {
				return nil, Errorf(http.StatusConflict, "%s is already a site here", domain)
			}
		}
		if s.opts.Auth == nil {
			return nil, Errorf(http.StatusNotImplemented, "no token store")
		}
		pub, secret, err := s.opts.Auth.CreateExpiringToken("import "+domain, []string{"import:" + domain}, 2*time.Hour)
		if err != nil {
			return nil, err
		}
		// The address the V1 machine reaches this house at: the one asked
		// for, or this node's own panel domain. Never the request's Host:
		// a node opened from another node's panel is reached through the
		// mesh, and its mesh address (10.66.x.y:46101) is no way in from
		// the V1 machine; the first import printed exactly that.
		base := strings.TrimRight(strings.TrimSpace(body.Base), "/")
		if base == "" && s.opts.Settings != nil {
			if d := s.opts.Settings().PanelDomain; d != "" {
				base = "https://" + d
			}
		}
		if base == "" {
			return nil, Errorf(http.StatusBadRequest, "this node has no panel domain of its own, so the V1 machine has no way to reach it: give the address it can reach (base), or import into a node that has one and move the site here afterwards")
		}
		if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
			base = "https://" + base
		}
		// This house's own release: every release so far is a pre-release,
		// and GitHub's "latest" link skips those.
		release := "latest/download"
		if v := s.opts.Version; strings.HasPrefix(v, "v2.") {
			release = "download/" + v
		}
		cmd := fmt.Sprintf("curl -fsSL -o /tmp/middleware-v2 https://github.com/dcimorra/middleware-public/releases/%s/middleware-linux-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/') && chmod +x /tmp/middleware-v2 && /tmp/middleware-v2 import --to %s --token %s --domain %s <the app's folder>", release, base, secret, domain)
		return map[string]any{"token": secret, "expires": pub.Expires, "domain": domain, "command": cmd}, nil
	})
}

// installDeps installs a received Node site's dependencies in its staging
// folder, with the Node the site asks for (installed if need be). The
// lockfile says the tool; with none, npm install.
func installDeps(ctx context.Context, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err == nil {
		return nil
	}
	argv := []string{"npm", "install", "--no-audit", "--no-fund"}
	switch {
	case fileThere(filepath.Join(dir, "package-lock.json")):
		argv = []string{"npm", "ci", "--no-audit", "--no-fund"}
	case fileThere(filepath.Join(dir, "pnpm-lock.yaml")):
		argv = []string{"corepack", "pnpm", "install", "--frozen-lockfile"}
	case fileThere(filepath.Join(dir, "yarn.lock")):
		argv = []string{"corepack", "yarn", "install", "--frozen-lockfile"}
	}
	path := os.Getenv("PATH")
	bin := argv[0]
	if goruntime.NodeDir != nil {
		if nd := goruntime.NodeDir(ctx, dir, argv); nd != "" {
			path = nd + string(os.PathListSeparator) + path
			if fileThere(filepath.Join(nd, bin)) {
				bin = filepath.Join(nd, bin)
			}
		}
	}
	cmd := exec.CommandContext(ctx, bin, argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+path, "CI=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		tail := strings.TrimSpace(string(out))
		if len(tail) > 600 {
			tail = tail[len(tail)-600:]
		}
		return fmt.Errorf("%s: %v: %s", strings.Join(argv, " "), err, tail)
	}
	return nil
}

// siteNodeMajor is the Node major the site's command runs on here: the
// one nodeenv gives it, or the machine's own.
func siteNodeMajor(ctx context.Context, dir string, argv []string) string {
	node := "node"
	if goruntime.NodeDir != nil {
		if nd := goruntime.NodeDir(ctx, dir, argv); nd != "" {
			node = filepath.Join(nd, "node")
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, node, "--version").Output()
	if err != nil {
		return ""
	}
	v := strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
	major, _, _ := strings.Cut(v, ".")
	if _, err := strconv.Atoi(major); err != nil {
		return ""
	}
	return major
}

// installsThere says a moved site's node_modules is installed on the
// target rather than carried: a Node site on the machine. One in a
// container keeps it, built inside the same image on both sides.
func installsThere(dir string, m *manifest.Manifest) bool {
	if m == nil || m.Run == nil || m.Run.Runtime == "docker" {
		return false
	}
	return fileThere(filepath.Join(dir, "package.json"))
}

func fileThere(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
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
		cctx, ccancel := context.WithTimeout(r.Context(), 30*time.Second)
		err = (meshTarget{m: m, x: x}).Check(cctx, domain, relocate.NeedsOf(s.opts.Root, domain, src))
		ccancel()
		if err != nil {
			return nil, Errorf(http.StatusConflict, "%s cannot take %s: %v", x.Name, domain, err)
		}
		install := false
		if site, err := mgr.Get(domain); err == nil && installsThere(mgr.Dir(domain), site.Manifest) {
			install = true
			// The Node it runs on here goes with it, written in its
			// manifest, when nothing says it yet: a site that asks for none
			// runs on each machine's own node, and the target's was another
			// (24 here, 18 there: a native module built here did not load there).
			if site.Manifest.Node == "" {
				if v := siteNodeMajor(r.Context(), mgr.Dir(domain), site.Manifest.Run.Command); v != "" {
					raw, _ := json.Marshal(v)
					if _, err := mgr.Patch(domain, map[string]json.RawMessage{"node": raw}); err != nil {
						return nil, Errorf(http.StatusInternalServerError, "writing the Node it runs on (%s) in its manifest: %v", v, err)
					}
				}
			}
		}
		_, published := src.Published(domain)
		job, busy := s.newMove(domain, x.Name, movePlan(install, published))
		if busy != nil {
			return nil, Errorf(http.StatusConflict, "%s is already moving to %s (%s)", domain, busy.Node, busy.Step)
		}
		mover := &relocate.Mover{Root: s.opts.Root, Source: src}
		if install {
			mover.Install, mover.Parts = true, relocate.MoveParts
		}
		go func() {
			// A move outlives the request that asked for it: an hour for a
			// big site over a slow link.
			ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
			defer cancel()
			rep, err := mover.Move(ctx, domain, meshTarget{m: m, x: x}, func(step, msg string) {
				s.advance(job, step, msg)
				s.emitMove("relocate."+step, domain, "", msg, map[string]any{"node": x.Name, "id": job.ID})
			})
			s.finish(job, rep, err)
			if err != nil {
				s.emitMove("relocate.failed", domain, "error", err.Error(), map[string]any{"node": x.Name, "id": job.ID})
			}
		}()
		s.moves.mu.Lock()
		c := job.copy()
		s.moves.mu.Unlock()
		return c, nil
	})
	s.Handle("GET", Prefix+"/sites/{domain}/move", "The site's last move to another node: each operation with its state, and how it ended", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		if list := s.listMoves(domain); len(list) > 0 {
			return list[0], nil
		}
		return nil, Errorf(http.StatusNotFound, "%s has not been moved from this node", domain)
	})
	s.Handle("GET", Prefix+"/moves", "The moves from this node to others, the newest first (?domain= one site's): each operation with its state, its times and its error", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return s.listMoves(r.URL.Query().Get("domain")), nil
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

func (t meshTarget) Prepare(ctx context.Context, domain string) error {
	return t.older(t.m.Call(ctx, t.x, "POST", "/relocate/"+domain+"/prepare", nil, nil))
}

func (t meshTarget) Discard(ctx context.Context, domain string) error {
	return t.m.Call(ctx, t.x, "DELETE", "/relocate/"+domain, nil, nil)
}

func (t meshTarget) Remove(ctx context.Context, domain string) error {
	return t.m.Call(ctx, t.x, "DELETE", "/sites/"+domain, nil, nil)
}

func (t meshTarget) Check(ctx context.Context, domain string, need relocate.Needs) error {
	return t.older(t.m.Call(ctx, t.x, "POST", "/relocate/"+domain+"/check", need, nil))
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

// dockerAnswers says this machine has a docker whose daemon answers.
func dockerAnswers(ctx context.Context) error {
	bin, err := exec.LookPath("docker")
	if err != nil {
		return errors.New("docker is not installed")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, bin, "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		return fmt.Errorf("the daemon does not answer: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
