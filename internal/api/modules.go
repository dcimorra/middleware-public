package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/modules"
)

// ModulesOptions wire the modules section.
type ModulesOptions struct {
	Manager *modules.Manager
	// Public lists module routes that need no caller, as "METHOD key/path"
	// ("POST github/hook"): a webhook authenticates with its own signature
	// and GitHub holds no token of ours.
	Public []string
	// EnableTimeout bounds a verify-install-enable; a download of a
	// connector binary is the slow case. Default five minutes.
	EnableTimeout time.Duration
}

// RegisterModules adds /modules and mounts every module's routes under
// /modules/{key}/. The specific routes are registered first; the mount is
// a prefix pattern and the Go 1.22 mux picks the most specific one, so
// "POST /modules/vpn/verify" is the manager's and "POST /modules/vpn/peers"
// is the module's.
func (s *Server) RegisterModules(o ModulesOptions) {
	mgr := o.Manager
	if o.EnableTimeout == 0 {
		o.EnableTimeout = 5 * time.Minute
	}
	p := Prefix + "/modules"
	s.Handle("GET", p, "List the modules and their state", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return mgr.List(), nil
	})
	s.Handle("GET", p+"/{key}", "One module: its state, or its own status when it is enabled and has one", func(w http.ResponseWriter, r *http.Request) (any, error) {
		key, err := moduleKey(r)
		if err != nil {
			return nil, err
		}
		if mod, ok := mgr.Get(key); ok && mgr.Enabled(key) {
			if h := statusRoute(mod); h != nil {
				r2 := r.Clone(r.Context())
				r2.URL.Path = "/"
				h.ServeHTTP(w, r2)
				return Handled, nil
			}
		}
		for _, info := range mgr.List() {
			if info.Key == key {
				return info, nil
			}
		}
		return nil, Errorf(http.StatusNotFound, "no module %q", key)
	})
	s.Handle("POST", p+"/{key}/verify", "Probe a module's credentials without saving them", func(w http.ResponseWriter, r *http.Request) (any, error) {
		key, cfg, err := moduleBody(r)
		if err != nil {
			return nil, err
		}
		lines, err := mgr.Verify(r.Context(), key, cfg)
		if err != nil {
			return nil, moduleError(err)
		}
		return map[string]any{"ok": !modules.Failed(lines), "lines": lines}, nil
	})
	s.Handle("POST", p+"/{key}/enable", "Verify, install, enable and save a module", func(w http.ResponseWriter, r *http.Request) (any, error) {
		key, cfg, err := moduleBody(r)
		if err != nil {
			return nil, err
		}
		// The request's context would end the install when the panel's
		// tab closes; the operator pressed the button and expects the
		// module to come up whether or not they wait for it.
		ctx, cancel := context.WithTimeout(context.Background(), o.EnableTimeout)
		defer cancel()
		var mu sync.Mutex
		var progress []string
		lines, err := mgr.Enable(ctx, key, cfg, func(line string) {
			mu.Lock()
			progress = append(progress, line)
			mu.Unlock()
		})
		if err != nil {
			if errors.Is(err, modules.ErrVerifyFailed) {
				return map[string]any{"ok": false, "lines": lines, "progress": progress, "error": err.Error()}, nil
			}
			return nil, moduleError(err)
		}
		return map[string]any{"ok": true, "lines": lines, "progress": progress}, nil
	})
	s.Handle("POST", p+"/{key}/disable", "Turn a module off, keeping its credentials", func(w http.ResponseWriter, r *http.Request) (any, error) {
		key, err := moduleKey(r)
		if err != nil {
			return nil, err
		}
		if err := mgr.Disable(r.Context(), key); err != nil {
			return nil, moduleError(err)
		}
		return OK, nil
	})
	s.Handle("DELETE", p+"/{key}/credentials", "Turn a module off and delete its credentials", func(w http.ResponseWriter, r *http.Request) (any, error) {
		key, err := moduleKey(r)
		if err != nil {
			return nil, err
		}
		if err := mgr.Forget(r.Context(), key); err != nil {
			return nil, moduleError(err)
		}
		return OK, nil
	})
	dispatcher := http.StripPrefix(p, moduleRoutes(mgr))
	s.Mount(p, dispatcher)
	for _, pub := range o.Public {
		method, rel, ok := strings.Cut(pub, " ")
		if !ok {
			continue
		}
		rel = strings.TrimPrefix(rel, "/")
		pattern := p + "/" + rel
		// A public route must be its own registration: the gate looks up
		// the matched pattern, and the mount's prefix pattern is never
		// public.
		s.Handle(method, pattern, "Module route open to the world (authenticated by its own signature)", func(w http.ResponseWriter, r *http.Request) (any, error) {
			dispatcher.ServeHTTP(w, r)
			return Handled, nil
		})
		s.Public(method, pattern)
	}
}

// moduleRoutes dispatches /<key>/<rest> to the module's routes. Routes of
// a disabled module answer 404, so a webhook is rejected the moment the
// operator turns the module off. Two key styles are accepted: plain path
// keys ("hook", what github and cloudflare declare) matched exactly, and
// ServeMux patterns ("GET /{$}", "DELETE /peers/{name}", what the vpn
// declares) served through a mux built once per module.
func moduleRoutes(mgr *modules.Manager) http.Handler {
	var muxes sync.Map // key → *http.ServeMux
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		mod, ok := mgr.Get(key)
		if !ok || (!mgr.Enabled(key) && !setupRoute(mod, r.Method, rest)) {
			WriteJSON(w, http.StatusNotFound, map[string]string{"error": "no such module route"})
			return
		}
		routes := mod.Routes()
		if len(routes) == 0 {
			WriteJSON(w, http.StatusNotFound, map[string]string{"error": "the module has no routes"})
			return
		}
		if !patternRoutes(routes) {
			h, ok := routes[strings.TrimSuffix(rest, "/")]
			if !ok {
				WriteJSON(w, http.StatusNotFound, map[string]string{"error": "no such module route"})
				return
			}
			h.ServeHTTP(w, r)
			return
		}
		v, ok := muxes.Load(key)
		if !ok {
			mux := http.NewServeMux()
			for pattern, h := range routes {
				mux.Handle(pattern, h)
			}
			v, _ = muxes.LoadOrStore(key, mux)
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + rest
		r2.URL.RawPath = ""
		v.(*http.ServeMux).ServeHTTP(w, r2)
	})
}

// setupRoute says whether a disabled module answers this request: the
// module lists the patterns ("POST /login") that make sense before it is
// enabled.
func setupRoute(mod modules.Module, method, rest string) bool {
	s, ok := mod.(modules.Setup)
	if !ok {
		return false
	}
	path := "/" + strings.TrimSuffix(rest, "/")
	for _, pat := range s.SetupRoutes() {
		m, p, found := strings.Cut(pat, " ")
		if !found {
			m, p = "", pat
		}
		if (m == "" || m == method) && (p == path || (p == "/{$}" && path == "/")) {
			return true
		}
	}
	return false
}

// patternRoutes reports whether the keys are mux patterns.
func patternRoutes(routes map[string]http.Handler) bool {
	for k := range routes {
		if strings.HasPrefix(k, "/") || strings.Contains(k, " ") {
			return true
		}
	}
	return false
}

// statusRoute finds the handler for the module's root, in either style.
func statusRoute(mod modules.Module) http.Handler {
	routes := mod.Routes()
	for _, k := range []string{"", "GET /{$}", "/{$}", "GET /"} {
		if h, ok := routes[k]; ok {
			return h
		}
	}
	return nil
}

func moduleKey(r *http.Request) (string, error) {
	key := strings.TrimSpace(r.PathValue("key"))
	if !modules.ValidKey(key) {
		return "", Errorf(http.StatusBadRequest, "%q is not a module key", key)
	}
	return key, nil
}

// moduleBody reads {config: {...}}; a missing body is an empty config,
// which the manager fills from the store.
func moduleBody(r *http.Request) (string, modules.Config, error) {
	key, err := moduleKey(r)
	if err != nil {
		return "", nil, err
	}
	var body struct {
		Config modules.Config `json:"config"`
	}
	if r.ContentLength != 0 && r.Body != nil {
		if err := ReadJSON(r, &body); err != nil {
			return "", nil, err
		}
	}
	if body.Config == nil {
		body.Config = modules.Config{}
	}
	return key, body.Config, nil
}

// moduleError maps the manager's errors: an unknown key is 404, a value
// the module does not ask for or a missing required one is 400, and a
// failed install or enable is 502 because the fault is on the far side.
func moduleError(err error) error {
	switch {
	case errors.Is(err, modules.ErrUnknown):
		return Errorf(http.StatusNotFound, "%v", err)
	case errors.Is(err, modules.ErrVerifyFailed):
		return Errorf(http.StatusBadRequest, "%v", err)
	}
	msg := err.Error()
	if strings.Contains(msg, "is not something this module asks for") || strings.Contains(msg, "is required") {
		return Errorf(http.StatusBadRequest, "%s", msg)
	}
	if strings.HasPrefix(msg, "install:") || strings.HasPrefix(msg, "enable:") {
		return Errorf(http.StatusBadGateway, "%s", msg)
	}
	return err
}
