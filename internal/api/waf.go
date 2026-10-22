package api

import (
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/attack"
	"github.com/dcimorra/middleware-public/internal/traffic"
	"github.com/dcimorra/middleware-public/internal/waf"
)

// WAFOptions wire the WAF section. Either may be nil when the package did
// not start (a broken block list); the routes then answer 503 and say so
// rather than panic on a nil.
type WAFOptions struct {
	WAF     *waf.WAF
	Traffic *traffic.Recorder
	// Attack holds the challenge counters; nil when it did not start.
	Attack *attack.Guard
}

// RegisterWAF adds the /waf routes.
func (s *Server) RegisterWAF(o WAFOptions) {
	p := Prefix + "/waf"
	need := func() (*waf.WAF, error) {
		if o.WAF == nil {
			return nil, Errorf(http.StatusServiceUnavailable, "the waf did not start; see the waf.error event")
		}
		return o.WAF, nil
	}
	s.Handle("GET", p, "The global policy, the block list and the recent offenders", func(w http.ResponseWriter, r *http.Request) (any, error) {
		wf, err := need()
		if err != nil {
			return nil, err
		}
		blocked, recent := wf.Blocked(), wf.Recent()
		if blocked == nil {
			blocked = []waf.Entry{}
		}
		if recent == nil {
			recent = []waf.Hit{}
		}
		att := map[string]any{"global": wf.GlobalAttack(), "counts": map[string]attack.Counter{}}
		if o.Attack != nil {
			att["counts"] = o.Attack.Counts()
		}
		pol := wf.Policy()
		return map[string]any{"global": pol, "blocked": blocked, "recent": recent, "attack": att,
			"ban": pol.Ban.Effective(), "watched": wf.Watched()}, nil
	})
	s.Handle("PUT", p, "Replace the global policy", func(w http.ResponseWriter, r *http.Request) (any, error) {
		wf, err := need()
		if err != nil {
			return nil, err
		}
		var pol waf.Policy
		if err := ReadJSON(r, &pol); err != nil {
			return nil, err
		}
		if err := wf.SetPolicy(pol); err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		return wf.Policy(), nil
	})
	s.Handle("POST", p+"/block", "Block an address", func(w http.ResponseWriter, r *http.Request) (any, error) {
		wf, err := need()
		if err != nil {
			return nil, err
		}
		var body struct {
			IP       string `json:"ip"`
			Duration string `json:"duration"`
			Reason   string `json:"reason"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		ip := net.ParseIP(strings.TrimSpace(body.IP))
		if ip == nil {
			return nil, Errorf(http.StatusBadRequest, "ip: %q is not an address", body.IP)
		}
		dur := 24 * time.Hour
		if body.Duration != "" {
			d, err := time.ParseDuration(body.Duration)
			if err != nil || d <= 0 {
				return nil, Errorf(http.StatusBadRequest, "duration: %q is not a positive duration", body.Duration)
			}
			dur = d
		}
		if body.Reason == "" {
			body.Reason = "blocked from the panel"
		}
		if err := wf.Block(ip, dur, body.Reason); err != nil {
			return nil, err
		}
		return OK, nil
	})
	s.Handle("DELETE", p+"/block/{ip}", "Unblock an address", func(w http.ResponseWriter, r *http.Request) (any, error) {
		wf, err := need()
		if err != nil {
			return nil, err
		}
		ip := net.ParseIP(r.PathValue("ip"))
		if ip == nil {
			return nil, Errorf(http.StatusBadRequest, "%q is not an address", r.PathValue("ip"))
		}
		if err := wf.Unblock(ip); err != nil {
			return nil, err
		}
		return OK, nil
	})
	s.Handle("POST", p+"/under-attack", "Switch the challenge on or off, globally or for one site", func(w http.ResponseWriter, r *http.Request) (any, error) {
		wf, err := need()
		if err != nil {
			return nil, err
		}
		// Every field but enabled is optional and left as it was when
		// absent. The deadline is "for" and is turned into a date here:
		// the clock that decides whether it has passed is this machine's,
		// not the browser's that asked. It is recomputed only when sent,
		// because a deadline that stretches whenever something else is
		// saved is not a deadline. "for": "" is permanent.
		var body struct {
			Enabled bool      `json:"enabled"`
			Domain  string    `json:"domain"`
			For     *string   `json:"for"`
			Bits    *int      `json:"bits"`
			Life    *string   `json:"life"`
			Skip    *[]string `json:"skip"`
			Message *string   `json:"message"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		until := ""
		if body.For != nil && *body.For != "" {
			d, err := time.ParseDuration(*body.For)
			if err != nil || d <= 0 {
				return nil, Errorf(http.StatusBadRequest, "for: %q is not a positive duration", *body.For)
			}
			until = time.Now().Add(d).UTC().Format(time.RFC3339)
		}
		err = wf.SetAttack(strings.ToLower(strings.TrimSpace(body.Domain)), func(a *attack.Rules) {
			a.On = body.Enabled
			if body.For != nil {
				a.Until = until
			}
			if body.Bits != nil {
				a.Bits = *body.Bits
			}
			if body.Life != nil {
				a.Life = *body.Life
			}
			if body.Skip != nil {
				a.Skip = *body.Skip
			}
			if body.Message != nil {
				a.Message = *body.Message
			}
		})
		switch {
		case err != nil && strings.HasSuffix(err.Error(), "no such site"):
			return nil, Errorf(http.StatusNotFound, "%v", err)
		case err != nil && strings.Contains(err.Error(), "attack."):
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		case err != nil:
			return nil, err
		}
		return OK, nil
	})
	s.Handle("GET", p+"/traffic", "The last minute of traffic", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if o.Traffic == nil {
			return nil, Errorf(http.StatusServiceUnavailable, "traffic is not recorded")
		}
		return o.Traffic.Snapshot(), nil
	})
}
