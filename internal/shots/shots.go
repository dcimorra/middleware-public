// Package shots takes the screenshots the panel shows of each site.
//
// One at a time, on purpose: a browser is the most expensive thing this
// process can start, and twenty at once would cost more memory than the
// sites it serves. They are renewed every half hour only while someone has
// looked at the panel in the last hour: V1 renewed them day and night, a
// new browser per site every half hour, which was more CPU than the whole
// supervisor at rest, for pictures nobody was looking at.
//
// The picture is taken through a door opened for it: the site as it is
// once past the house's own doors, for the seconds the browser takes and
// for that domain alone. Taken through the front door, every private site's
// thumbnail was its password form, and the address and country filters
// turned the browser away because it never came from outside.
package shots

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
)

// NoBrowser says what to do when there is no browser.
const NoBrowser = "no headless Chromium on this machine: run the installer again and answer yes to the browser (middleware install --browser yes), or set MIDDLEWARE_CHROME"

// Refresh is how often a picture is renewed while the panel is watched.
const Refresh = 30 * time.Minute

// WatchedFor is how long after the panel last asked pictures keep renewing.
const WatchedFor = time.Hour

// Options configure a Shotter.
type Options struct {
	DataDir string
	Bus     *events.Bus
	// Bare serves a site with nothing of the house in front (the proxy
	// without its hooks).
	Bare http.Handler
	// Shootable says whether a picture now would show the site and not an
	// error or holding page, which would overwrite the last good one.
	Shootable func(domain string) bool
	// Domains lists what may be photographed.
	Domains func() []string
	// Binary is the browser; empty finds one (see FindIn), and keeps
	// looking: a browser the installer puts there later is used without
	// a restart.
	Binary string
	// Root is the installation, where the installer puts its browser.
	Root string
}

// Shotter takes the pictures.
type Shotter struct {
	opts    Options
	binMu   sync.Mutex
	binary  string
	queue   chan req
	mu      sync.Mutex
	pending map[string]bool
	viewed  atomic.Int64
}

type req struct {
	domain string
	force  bool
}

// New builds a shotter; Run starts it.
func New(opts Options) *Shotter {
	if opts.Bus == nil {
		opts.Bus = events.New("")
	}
	return &Shotter{opts: opts, binary: opts.Binary, queue: make(chan req, 256), pending: map[string]bool{}}
}

// browser is the browser to use, looked for again while there is none.
func (s *Shotter) browser() string {
	s.binMu.Lock()
	defer s.binMu.Unlock()
	if s.binary == "" {
		s.binary = FindIn(s.opts.Root)
	}
	return s.binary
}

// Available says whether there is a browser to take pictures with.
func (s *Shotter) Available() bool { return s.browser() != "" }

// BrowserDir is where the installer puts Chrome for Testing's headless
// shell, inside the installation: no system path, nothing of root's.
func BrowserDir(root string) string { return filepath.Join(root, "modules", "chromium") }

// FindIn is Find with the installer's browser looked for first.
func FindIn(root string) string {
	if root != "" {
		matches, _ := filepath.Glob(filepath.Join(BrowserDir(root), "chrome-headless-shell-linux*", "chrome-headless-shell"))
		sort.Strings(matches)
		for i := len(matches) - 1; i >= 0; i-- {
			if exists(matches[i]) {
				return matches[i]
			}
		}
	}
	return Find()
}

// Find looks for a headless Chromium: MIDDLEWARE_CHROME, the newest one
// Playwright installed (its headless shell is the cheapest), then the
// system's.
func Find() string {
	if p := os.Getenv("MIDDLEWARE_CHROME"); exists(p) {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, pattern := range []string{
		"chromium_headless_shell-*/chrome-headless-shell-linux*/chrome-headless-shell",
		"chromium_headless_shell-*/chrome-linux*/headless_shell",
		"chromium-*/chrome-linux*/chrome",
	} {
		matches, _ := filepath.Glob(filepath.Join(home, ".cache", "ms-playwright", pattern))
		sort.Strings(matches)
		for i := len(matches) - 1; i >= 0; i-- {
			if exists(matches[i]) {
				return matches[i]
			}
		}
	}
	for _, p := range []string{"/usr/bin/chromium", "/usr/bin/chromium-browser", "/usr/bin/google-chrome", "/snap/bin/chromium"} {
		if exists(p) {
			return p
		}
	}
	return ""
}

func exists(p string) bool {
	st, err := os.Stat(p)
	return p != "" && err == nil && !st.IsDir()
}

// Path is the picture of domain, and when it was taken.
func (s *Shotter) Path(domain string) (string, time.Time, bool) {
	p := filepath.Join(s.opts.DataDir, "shots", domain+".png")
	st, err := os.Stat(p)
	if err != nil {
		return "", time.Time{}, false
	}
	return p, st.ModTime(), true
}

// Viewed marks the panel as watched: pictures keep renewing for an hour.
func (s *Shotter) Viewed() { s.viewed.Store(time.Now().Unix()) }

// Request queues a picture of domain; force takes it even when the site
// does not look shootable (the operator pressed the button).
func (s *Shotter) Request(domain string, force bool) {
	if !s.Available() {
		return
	}
	s.mu.Lock()
	if s.pending[domain] {
		s.mu.Unlock()
		return
	}
	s.pending[domain] = true
	s.mu.Unlock()
	select {
	case s.queue <- req{domain, force}:
	default:
		s.mu.Lock()
		delete(s.pending, domain)
		s.mu.Unlock()
	}
}

// Run takes queued pictures one at a time and renews them while watched.
func (s *Shotter) Run(ctx context.Context) {
	if !s.Available() {
		s.opts.Bus.Emit(events.Event{Type: "shots.off", Actor: "engine", Level: "warn", Message: NoBrowser + ": the panel shows no site pictures"})
	}
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-s.queue:
			s.capture(ctx, r)
			s.mu.Lock()
			delete(s.pending, r.domain)
			s.mu.Unlock()
		case <-t.C:
			if time.Since(time.Unix(s.viewed.Load(), 0)) > WatchedFor || s.opts.Domains == nil {
				continue
			}
			for _, d := range s.opts.Domains() {
				if _, at, ok := s.Path(d); !ok || time.Since(at) > Refresh {
					s.Request(d, false)
				}
			}
		}
	}
}

func (s *Shotter) capture(ctx context.Context, r req) {
	if !r.force && s.opts.Shootable != nil && !s.opts.Shootable(r.domain) {
		return
	}
	tmp, err := os.MkdirTemp("", "middleware-shot-")
	if err != nil {
		return
	}
	defer os.RemoveAll(tmp)
	port, shut, err := s.door(r.domain)
	if err != nil {
		return
	}
	defer shut()
	out := filepath.Join(tmp, "shot.png")
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	bin := s.browser()
	if bin == "" {
		return
	}
	cmd := exec.CommandContext(cctx, bin,
		"--headless", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage", "--hide-scrollbars",
		"--force-color-profile=srgb", "--user-data-dir="+filepath.Join(tmp, "profile"), "--window-size=1200,750",
		// Page time, not wall time: reproducible, and long enough for an
		// entrance animation to have finished (V1 learned it from a front
		// page whose curtain made every picture an empty rectangle).
		"--virtual-time-budget=15000",
		// Without this the picture is whatever the compositor had drawn,
		// which on a page still settling is half of it.
		"--run-all-compositor-stages-before-draw",
		// The real hostname, so the site sees the Host it always sees, but
		// through the door opened above.
		"--host-resolver-rules=MAP "+r.domain+" 127.0.0.1:"+strconv.Itoa(port),
		"--screenshot="+out, "http://"+r.domain+"/")
	cmd.Env = []string{"HOME=" + tmp, "PATH=/usr/bin:/bin"}
	if err := cmd.Run(); err != nil {
		s.opts.Bus.Emit(events.Event{Type: "shot.failed", Subject: r.domain, Actor: "engine", Level: "warn", Message: err.Error()})
		return
	}
	data, err := os.ReadFile(out)
	if err != nil || len(data) == 0 {
		return
	}
	final := filepath.Join(s.opts.DataDir, "shots", r.domain+".png")
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return
	}
	if os.WriteFile(final+".tmp", data, 0o644) == nil && os.Rename(final+".tmp", final) == nil {
		s.opts.Bus.Emit(events.Event{Type: "shot.taken", Subject: r.domain, Actor: "engine"})
	}
}

// door serves domain bare on a loopback port for one picture. The Host is
// checked too: the door was opened for one site and answers for one site.
func (s *Shotter) door(domain string) (int, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, nil, err
	}
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != domain {
			http.NotFound(w, r)
			return
		}
		s.opts.Bare.ServeHTTP(w, r)
	})}
	go srv.Serve(ln)
	return ln.Addr().(*net.TCPAddr).Port, func() { _ = srv.Close() }, nil
}
