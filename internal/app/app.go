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
	"sync"
	"time"

	"github.com/dcimorra/middleware-public/internal/engine"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/ports"
	"github.com/dcimorra/middleware-public/internal/proxy"
	"github.com/dcimorra/middleware-public/internal/settings"
)

// App is the process.
type App struct {
	Root string
	// Settings is what was loaded at start. It is read directly only
	// while the app is wired, before Run: from then on PUT /settings
	// replaces it from a request's goroutine while the agents' daily
	// budget, the doors and the listener read it from theirs; assigned
	// bare, that was a data race. Readers then go through CurrentSettings
	// and the one writer through SetSettings.
	Settings   settings.Settings
	settingsMu sync.RWMutex
	Bus        *events.Bus
	Engine     *engine.Engine
	Proxy      *proxy.Handler
	// Handler is the full request chain; hooks wrap Proxy in order.
	Handler http.Handler
	// Hooks let other packages wrap the handler and run alongside.
	Hooks []Hook
}

// CurrentSettings is the settings as last saved.
func (a *App) CurrentSettings() settings.Settings {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return a.Settings
}

// SetSettings replaces the settings everyone reads. The value is replaced
// whole, never changed in place, so the maps and pointers a reader's copy
// shares with it stay as they were for that reader.
func (a *App) SetSettings(s settings.Settings) {
	a.settingsMu.Lock()
	a.Settings = s
	a.settingsMu.Unlock()
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
	a.Proxy.SetPanelDomain(s.PanelDomain)
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
	h = recoverer(a.Bus, h)
	a.Handler = h
	a.watchDiag(ctx)
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
	err = srv.Serve(ln)
	// Hooks that keep something in memory write it now, before Run
	// returns: a goroutine left to do it on ctx.Done raced the process's
	// exit (and a test's cleanup of its root).
	for _, hook := range a.Hooks {
		if s, ok := hook.(Stopper); ok {
			s.Stop()
		}
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Stopper is a hook with something to write when the process stops.
type Stopper interface{ Stop() }

func (a *App) listener() (net.Listener, error) {
	if os.Getenv("LISTEN_FDS") == "1" {
		f := os.NewFile(3, "systemd-socket")
		ln, err := net.FileListener(f)
		if err == nil {
			return ln, nil
		}
	}
	return net.Listen("tcp", ListenAddr(a.CurrentSettings().Listen))
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
