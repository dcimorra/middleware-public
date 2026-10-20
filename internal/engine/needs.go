package engine

import (
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/manifest"
)

// What a site needs to do its job: another site, or one of a site's
// workers ("api@shop.example"). The dependencies existed before the field
// did, only in someone's head: playwithme talks to z4 over its port,
// litterae's frontend hands /api to its backend. When the lower one is
// down, the upper one shows "not answering", which is true and points at
// the wrong place.
//
// So `needs` does two things. A site starting for the first time waits
// (up to needsWait) for what it needs, so a whole machine coming up
// starts in order. And every snapshot names what is missing right now, so
// the panel says whose fault it is.
//
// What it does not do is keep a site from starting: past the wait it
// starts anyway and says so. A supervisor that refuses to bring a site up
// because another is down turns one outage into two.
const needsWait = 60 * time.Second

func splitNeed(v string) (site, worker string) {
	v = strings.TrimSpace(v)
	if name, dom, ok := strings.Cut(v, "@"); ok {
		return strings.TrimSpace(dom), strings.TrimSpace(name)
	}
	return v, ""
}

// missingLocked lists what s needs that is not available now, each with
// why. Called under e.mu.
func (e *Engine) missingLocked(s *Site) []string {
	if s.Manifest == nil {
		return nil
	}
	var out []string
	for _, need := range s.Manifest.Needs {
		domain, worker := splitNeed(need)
		other, ok := e.sites[domain]
		switch {
		case !ok || other.Manifest == nil:
			out = append(out, need+": no such site here")
		case worker != "":
			w, ok := other.Workers[worker]
			if !ok || w.inst == nil {
				out = append(out, need+": the worker is not running")
			}
		case other.Mode == manifest.ModeDisabled:
			out = append(out, need+": stopped")
		case other.health.status == HealthDown:
			out = append(out, need+": not answering")
		case other.Mode == manifest.ModeRun && other.Instance == nil:
			out = append(out, need+": not running")
		}
	}
	return out
}

// waitForNeeds decides whether a first start should wait. Called under
// e.mu; true means "not yet".
func (e *Engine) waitForNeeds(s *Site) bool {
	missing := e.missingLocked(s)
	if len(missing) == 0 {
		s.needsSince = time.Time{}
		return false
	}
	if s.needsSince.IsZero() {
		s.needsSince = time.Now()
		e.emit("needs.waiting", s.Domain, "warn", "waiting before the first start: "+strings.Join(missing, "; "), nil)
		return true
	}
	if time.Since(s.needsSince) < needsWait {
		return true
	}
	s.needsSince = time.Time{}
	e.emit("needs.timeout", s.Domain, "warn", "starting anyway after "+needsWait.String()+": "+strings.Join(missing, "; "), nil)
	return false
}
