package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Health is what the last checks say about a site.
type Health string

// The states. Unknown is not a polite "ok": a site on hold, disabled or not
// yet checked is not asserted to be well, because a panel that paints a
// stopped site green is saying something nobody checked.
const (
	HealthUnknown Health = ""
	HealthOK      Health = "ok"
	HealthDown    Health = "down"
)

// Check is one health probe, kept for the panel to draw.
type Check struct {
	At     time.Time `json:"at"`
	OK     bool      `json:"ok"`
	Millis int64     `json:"ms"`
	Error  string    `json:"error,omitempty"`
}

const (
	defaultHealthInterval = 30 * time.Second
	maxChecks             = 60
	// Asking a site whether it is well wakes its process. A site that has
	// answered well for a long streak says less each time, so the delay
	// stretches up to maxSpacing times the interval; any failure brings it
	// straight back. A site that dies is seen at once anyway (its process
	// exits); what spacing delays is noticing one that hangs while alive.
	maxSpacing  = 4
	hitsPerStep = 10
)

// healthState is the per-site bookkeeping.
type healthState struct {
	status   Health
	hits     int
	checks   []Check
	next     time.Time
	checking bool
}

func (h *healthState) delay(base time.Duration) time.Duration {
	if h.status != HealthOK {
		return base
	}
	f := 1 + h.hits/hitsPerStep
	if f > maxSpacing {
		f = maxSpacing
	}
	return time.Duration(f) * base
}

// healthDue starts a check for s when one is due. Called under e.mu; the
// probe itself runs outside the lock.
func (e *Engine) healthDue(ctx context.Context, s *Site) {
	h := &s.health
	if h.checking || time.Now().Before(h.next) {
		return
	}
	var probe func() Check
	var interval time.Duration
	switch s.Mode {
	case manifest.ModeRun:
		if s.Instance == nil || s.handing || s.Spec == nil {
			// Starting, crashed or backing off: the engine already knows
			// and says so through Error; a probe would only add noise.
			return
		}
		port, hc := s.Instance.Port(), s.Spec.Health
		interval = hc.Interval.Or(defaultHealthInterval)
		probe = func() Check { return probeHTTP(ctx, port, hc) }
	case manifest.ModeStatic:
		dir, m := s.Dir, s.Manifest
		interval = defaultHealthInterval
		probe = func() Check { return probeFiles(dir, m) }
	default:
		e.setHealth(s, HealthUnknown, nil)
		return
	}
	h.checking = true
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		c := probe()
		e.mu.Lock()
		h.checking = false
		e.mu.Unlock()
		e.record(s, c, interval)
	}()
}

func (e *Engine) record(s *Site, c Check, interval time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := &s.health
	h.checks = append(h.checks, c)
	if len(h.checks) > maxChecks {
		h.checks = h.checks[len(h.checks)-maxChecks:]
	}
	status := HealthDown
	if c.OK {
		h.hits++
		status = HealthOK
	} else {
		h.hits = 0
	}
	e.setHealth(s, status, &c)
	h.next = time.Now().Add(h.delay(interval))
}

// setHealth records a status and says so when it changes. Called under e.mu.
func (e *Engine) setHealth(s *Site, status Health, c *Check) {
	if s.health.status == status {
		return
	}
	s.health.status = status
	if status == HealthUnknown {
		s.health.hits = 0
	}
	level, msg, typ := "", "answering", "health.ok"
	switch status {
	case HealthDown:
		level, msg, typ = "error", "not answering", "health.down"
		if c != nil && c.Error != "" {
			msg += ": " + c.Error
		}
	case HealthUnknown:
		msg, typ = "not checked while "+string(s.Mode), "health.unknown"
	}
	e.emit(typ, s.Domain, level, msg, nil)
}

// healthClient never follows redirects: a 301 is a healthy answer, and
// following it would check whatever it points at instead of this site.
var healthClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		DialContext:       (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		DisableKeepAlives: true,
	},
}

func probeHTTP(ctx context.Context, port int, hc manifest.Health) Check {
	start := time.Now()
	c := Check{At: start}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	timeout := hc.Timeout.Or(8 * time.Second)
	if timeout > 30*time.Second {
		// The start timeout can be minutes (a cold Next build); a
		// running site that takes that long to answer is down.
		timeout = 30 * time.Second
	}
	if hc.Path == "" {
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			c.Error = err.Error()
		} else {
			conn.Close()
			c.OK = true
		}
		c.Millis = time.Since(start).Milliseconds()
		return c
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, "http://"+addr+hc.Path, nil)
	req.Header.Set("User-Agent", "middleware-health")
	resp, err := healthClient.Do(req)
	c.Millis = time.Since(start).Milliseconds()
	if err != nil {
		c.Error = err.Error()
		return c
	}
	resp.Body.Close()
	if !hc.Healthy(resp.StatusCode) {
		c.Error = fmt.Sprintf("answered %d", resp.StatusCode)
		return c
	}
	c.OK = true
	return c
}

// probeFiles is a static site's check: it has no process, so the only
// thing that can be wrong is that the files are not there.
func probeFiles(dir string, m *manifest.Manifest) Check {
	start := time.Now()
	c := Check{At: start}
	root := dir
	if m != nil && m.Static != nil && m.Static.Root != "" {
		root = filepath.Join(dir, m.Static.Root)
	} else if st, err := os.Stat(filepath.Join(dir, "public")); err == nil && st.IsDir() {
		root = filepath.Join(dir, "public")
	}
	if _, err := os.Stat(filepath.Join(root, "index.html")); err != nil {
		c.Error = "no index.html in " + root
	} else {
		c.OK = true
	}
	c.Millis = time.Since(start).Milliseconds()
	return c
}
