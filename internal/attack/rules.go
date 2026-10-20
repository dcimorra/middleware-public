// Package attack is under-attack mode: before anything is served, the
// browser proves some work (SHA-256 until a number gives a hash that
// starts with so many zero bits) and gets a signed pass for a while.
//
// The only thing it decides is the price of the first request. A browser
// pays a fraction of a second once per pass; a script asking a thousand
// times a second has to solve a thousand challenges a second, while
// checking one costs this house one hash. It guards nothing: whoever runs
// JavaScript gets through, as with Cloudflare's. That is why it exists
// next to the edge's: only the names proxied by the edge get the edge's,
// and a house serving through its own 80 and 443 has nothing between the
// internet and a Node process otherwise.
//
// Challenge and pass are signed with a key kept on disk and nothing is
// kept in memory: a restart of the supervisor must not send everyone who
// was inside back to solve again, which would be the very flood this
// protects from, caused by us.
package attack

import (
	"fmt"
	"strings"
	"time"
)

// Rules is the manifest's `attack` section, and the global one in the
// WAF policy.
type Rules struct {
	On bool `json:"on"`
	// Bits is the difficulty: each bit doubles the work. Measured in V1:
	// 9 ms at 12, 124 ms at 16, 1.6 s at 20.
	Bits int `json:"bits,omitempty"`
	// Life is how long a pass lasts, as a Go duration ("30m").
	Life string `json:"life,omitempty"`
	// Until switches it off by itself (RFC 3339). Empty is permanent, and
	// the panel says so: an attack lasts an afternoon, and what happens
	// after is that nobody remembers to switch it off, so weeks later
	// every visitor still pays and every webhook still bounces.
	Until string `json:"until,omitempty"`
	// Skip are path prefixes (whole segments) that are never challenged:
	// a webhook, a payment callback, an API. Every line is an open door.
	Skip []string `json:"skip,omitempty"`
	// Message replaces the line the visitor reads while it works.
	Message string `json:"message,omitempty"`
}

const (
	defaultBits = 16
	minBits     = 8
	// maxBits stays at 24 because the browser compares the first 32-bit
	// word of the hash and nothing more, and because past that a phone
	// takes minutes.
	maxBits     = 24
	defaultLife = 30 * time.Minute
)

// Validate rejects rules the guard could not apply.
func (r *Rules) Validate() error {
	if r == nil {
		return nil
	}
	if r.Bits != 0 && (r.Bits < minBits || r.Bits > maxBits) {
		return fmt.Errorf("bits: %d is not between %d and %d", r.Bits, minBits, maxBits)
	}
	if r.Life != "" {
		d, err := time.ParseDuration(r.Life)
		if err != nil || d < time.Minute || d > 7*24*time.Hour {
			return fmt.Errorf("life: %q is not a duration between 1m and 168h", r.Life)
		}
	}
	if r.Until != "" {
		if _, err := time.Parse(time.RFC3339, r.Until); err != nil {
			return fmt.Errorf("until: %q is not an RFC 3339 time", r.Until)
		}
	}
	for _, s := range r.Skip {
		if !strings.HasPrefix(s, "/") || strings.TrimRight(s, "/") == "" {
			return fmt.Errorf("skip: %q must be a path under /, not / itself", s)
		}
	}
	return nil
}

// Difficulty is Bits or the default.
func (r *Rules) Difficulty() int {
	if r == nil || r.Bits == 0 {
		return defaultBits
	}
	return r.Bits
}

// PassLife is Life or the default.
func (r *Rules) PassLife() time.Duration {
	if r != nil && r.Life != "" {
		if d, err := time.ParseDuration(r.Life); err == nil {
			return d
		}
	}
	return defaultLife
}

// Active says whether a challenge is charged now. The deadline is looked
// at here, on every request, not only where the engine cleans the
// manifest up: writing someone else's folder can fail (read-only, disk
// full), and a deadline that depended on that would not be one.
func (r *Rules) Active(now time.Time) bool {
	if r == nil || !r.On {
		return false
	}
	return !r.Expired(now)
}

// Expired says whether Until has passed.
func (r *Rules) Expired(now time.Time) bool {
	if r == nil || r.Until == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, r.Until)
	return err == nil && !now.Before(t)
}

// Skips says whether a path is exempt, by whole segments like every
// prefix in this house.
func (r *Rules) Skips(path string) bool {
	if r == nil {
		return false
	}
	for _, s := range r.Skip {
		base := strings.TrimRight(s, "/")
		if path == base || strings.HasPrefix(path, base+"/") {
			return true
		}
	}
	return false
}
