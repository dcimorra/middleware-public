// Package app wires the pieces into one running process. main is only
// argument parsing; everything that can be tested lives here.
package app

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dcimorra/middleware-public/internal/engine"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/ports"
	"github.com/dcimorra/middleware-public/internal/proxy"
	"github.com/dcimorra/middleware-public/internal/settings"
)

// App is the process.
type App struct {
	Root     string
	Settings settings.Settings
	Bus      *events.Bus
	Engine   *engine.Engine
	Proxy    *proxy.Handler
	// Handler is the full request chain; hooks wrap Proxy in order.
	Handler http.Handler
	// Hooks let other packages wrap the handler and run alongside.
	Hooks []Hook
}

// Hook is something that plugs into the app at start.
type Hook interface {
	// Wrap may return a handler around next.
	Wrap(next http.Handler) http.Handler
	// Run runs for the life of the process; it may return immediately.
	Run(ctx context.Context, a *App) error
}

// New loads settings and builds the core. Nothing runs yet.
func New(root string) (*App, error) {
	s, err := settings.Load(root)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{"sites", "tasks", "data", "modules"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	bus := events.New(filepath.Join(root, "data", "events"))
	eng := engine.New(engine.Options{
		SitesDir: filepath.Join(root, "sites"),
		DataDir:  filepath.Join(root, "data"),
		Ports:    ports.New(s.Ports.From, s.Ports.To),
		Bus:      bus,
	})
	a := &App{Root: root, Settings: s, Bus: bus, Engine: eng}
	a.Proxy = proxy.New(eng.Lookup)
	a.Proxy.PanelDomain = s.PanelDomain
	a.Handler = a.Proxy
	return a, nil
}

// Run starts the engine, the loopback listener and every hook, and blocks
// until ctx ends. The listener is taken from systemd's socket activation
// when LISTEN_FDS is set, so a restart never drops a connection waiting in
// the kernel's accept queue.
func (a *App) Run(ctx context.Context) error {
	h := a.Handler
	for i := len(a.Hooks) - 1; i >= 0; i-- {
		h = a.Hooks[i].Wrap(h)
	}
	a.Handler = h
	go a.Engine.Run(ctx)
	for _, hook := range a.Hooks {
		if err := hook.Run(ctx, a); err != nil {
			return err
		}
	}
	ln, err := a.listener()
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 15 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	a.Bus.Emit(events.Event{Type: "middleware.started", Actor: "engine", Data: map[string]any{"listen": ln.Addr().String()}})
	log.Printf("listening on %s, root %s", ln.Addr(), a.Root)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (a *App) listener() (net.Listener, error) {
	if os.Getenv("LISTEN_FDS") == "1" {
		f := os.NewFile(3, "systemd-socket")
		ln, err := net.FileListener(f)
		if err == nil {
			return ln, nil
		}
	}
	return net.Listen("tcp", ListenAddr(a.Settings.Listen))
}

// ListenAddr is where the loopback listener binds: MIDDLEWARE_LISTEN when
// set, else the settings. The variable exists for containers: inside one,
// 127.0.0.1 is the container's own loopback, which a published port never
// reaches (the host's traffic comes in by eth0 and the kernel resets it),
// so the image binds 0.0.0.0 there while docker publishes the port on the
// host's loopback only. The settings file keeps saying 127.0.0.1 so that
// the same volume moved to a systemd installation stays loopback-only.
func ListenAddr(fromSettings string) string {
	if v := os.Getenv("MIDDLEWARE_LISTEN"); v != "" {
		return v
	}
	return fromSettings
}
