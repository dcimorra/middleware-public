package api

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dcimorra/middleware-public/internal/app"
	"github.com/dcimorra/middleware-public/internal/auth"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/settings"
	"github.com/dcimorra/middleware-public/internal/sites"
)

// SocketName is the unix socket under data/.
const SocketName = "api.sock"

// SocketPath is where the socket lives under a root.
func SocketPath(root string) string { return filepath.Join(root, "data", SocketName) }

// Hook plugs the API into the app: it answers the panel host and listens
// on the unix socket. Build it with NewHook from cmd/middleware.
type Hook struct {
	Server *Server
	// SiteOf says which site's agent turn a process runs under ("" for
	// none); the wiring sets it to the chat manager's. Without it the
	// marks in the processes' environments alone decide.
	SiteOf func(pid int) (string, error)
	socket net.Listener
}

// NewHook builds the server over the app's engine, bus and settings. The
// UI is left to the panel's own hook, which sets Server.UI; until then
// the panel host answers with a plain text page.
func NewHook(a *app.App, version string) *Hook {
	store := auth.New(a.Root, a.Bus)
	mgr := sites.New(a.Root, a.Engine, a.Bus)
	srv := New(Options{
		Root:     a.Root,
		Version:  version,
		Listen:   a.Settings.Listen,
		Auth:     store,
		Sites:    mgr,
		Engine:   a.Engine,
		Bus:      a.Bus,
		Settings: a.CurrentSettings,
		SaveSettings: func(s settings.Settings) error {
			if err := settings.Save(a.Root, s); err != nil {
				return err
			}
			// What can be applied live is applied; the listener and the
			// port range need a restart and the panel says so. Under the
			// app's lock: the agents' daily budget and the doors read the
			// settings from their own goroutines meanwhile.
			a.SetSettings(s)
			a.Proxy.SetPanelDomain(s.PanelDomain)
			return nil
		},
	})
	return &Hook{Server: srv}
}

// Wrap implements app.Hook. The API does not wrap the request chain: it
// hangs from the proxy's panel slot so a site's host can never reach it.
func (h *Hook) Wrap(next http.Handler) http.Handler { return next }

// Run implements app.Hook: it installs the panel handler and opens the
// socket. The socket's requests are trusted (auth.WithSocket) because
// its mode lets only the service user and its group connect, and whoever
// can open it can also edit every manifest by hand. All but one: a site's
// agent turn runs as that same user, and it is confined to its site
// (SocketServer says how it is told apart).
func (h *Hook) Run(ctx context.Context, a *app.App) error {
	a.Proxy.Panel = h.Server.Panel()
	mgr := h.Server.opts.Sites
	if mgr != nil {
		mgr.Ctx = ctx
	}
	ln, err := ListenSocket(SocketPath(a.Root))
	if err != nil {
		return err
	}
	h.socket = ln
	srv := SocketServer(ctx, h.Server.Panel(), h.SiteOf)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		_ = os.Remove(ln.Addr().String())
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("api socket: %v", err)
			a.Bus.Emit(events.Event{Type: "api.socket_failed", Actor: "api", Level: "error", Message: err.Error()})
		}
	}()
	a.Bus.Emit(events.Event{Type: "api.listening", Actor: "api", Data: map[string]any{"socket": ln.Addr().String()}})
	return nil
}

// ListenSocket opens a unix socket at path with mode 0660, replacing a
// stale file from a previous run. A socket that still answers is not
// replaced: that would be a second supervisor, and the lock on the
// state is the first one's.
func ListenSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err == nil {
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, errors.New("another middleware is listening on " + path)
		}
		_ = os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
