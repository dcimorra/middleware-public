package api

import (
	"context"
	"net/http"
	"time"

	"github.com/dcimorra/middleware-public/internal/deps"
)

// DepsOptions wire the /deps section: the external CLIs the modules run.
type DepsOptions struct {
	Manager *deps.Manager
}

// RegisterDeps adds the routes that list, check and update the managed
// programs. A check or an update talks to the registry and can take a
// minute; both run under their own context so a closed tab does not
// abort an install halfway.
func (s *Server) RegisterDeps(o DepsOptions) {
	if o.Manager == nil {
		return
	}
	p := Prefix + "/deps"
	s.Handle("GET", p, "The external programs this installation manages: version, latest, whether an update is pending", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return o.Manager.Statuses(), nil
	})
	s.Handle("POST", p+"/check", "Ask the registry for newer versions now (and install them when auto-update is on)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		var progress []string
		sts := o.Manager.Check(ctx, func(line string) { progress = append(progress, line) })
		return map[string]any{"deps": sts, "progress": progress}, nil
	})
	s.Handle("POST", p+"/{name}/install", "Install or update one program to its latest release now", func(w http.ResponseWriter, r *http.Request) (any, error) {
		name := r.PathValue("name")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		var progress []string
		if err := o.Manager.Install(ctx, name, func(line string) { progress = append(progress, line) }); err != nil {
			return nil, Errorf(http.StatusBadGateway, "%v", err)
		}
		return map[string]any{"ok": true, "progress": progress, "deps": o.Manager.Statuses()}, nil
	})
}
