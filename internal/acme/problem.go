package acme

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Problem is the RFC 7807 document a CA answers with. It is the error type
// callers get, so a rate limit or a validation failure can be told apart
// from a network error without parsing strings.
type Problem struct {
	Type        string    `json:"type"`
	Detail      string    `json:"detail"`
	Status      int       `json:"status"`
	Instance    string    `json:"instance,omitempty"`
	Subproblems []Problem `json:"subproblems,omitempty"`
	// RetryAfter is the Retry-After header when the CA sent one; zero
	// otherwise.
	RetryAfter time.Duration `json:"-"`
}

const errPrefix = "urn:ietf:params:acme:error:"

// Error implements error.
func (p *Problem) Error() string {
	code := strings.TrimPrefix(p.Type, errPrefix)
	if code == "" {
		code = "unknown"
	}
	s := fmt.Sprintf("acme: %s (%d)", code, p.Status)
	if p.Detail != "" {
		s += ": " + p.Detail
	}
	for _, sub := range p.Subproblems {
		s += "; " + strings.TrimPrefix(sub.Type, errPrefix) + ": " + sub.Detail
	}
	return s
}

// IsCode reports whether the problem carries the given short code
// ("badNonce", "rateLimited", "unauthorized").
func (p *Problem) IsCode(code string) bool { return p.Type == errPrefix+code }

// RateLimited is the one error worth special treatment: retrying it sooner
// than Retry-After says makes the limit worse, and Let's Encrypt's weekly
// limits are not something an operator can lift by restarting.
func (p *Problem) RateLimited() bool {
	return p.IsCode("rateLimited") || p.Status == http.StatusTooManyRequests
}

// parseProblem reads a problem body, falling back to the status line when
// the CA answered with something that is not a problem document at all (a
// proxy's HTML page, for example).
func parseProblem(resp *http.Response, body []byte) *Problem {
	p := &Problem{}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") {
		_ = json.Unmarshal(body, p)
	}
	if p.Status == 0 {
		p.Status = resp.StatusCode
	}
	if p.Detail == "" && p.Type == "" {
		p.Detail = strings.TrimSpace(string(body))
		if len(p.Detail) > 200 {
			p.Detail = p.Detail[:200]
		}
	}
	p.RetryAfter = retryAfter(resp.Header)
	return p
}

// retryAfter understands both forms the header may take. A date in the past
// or an unparseable value reads as zero: callers then use their own pace.
func retryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
