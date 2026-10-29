// Package audit runs `npm audit` over each site once a day and says what
// is critical and new. It exists for the half of V1's incident no written
// rule fixes: the hole in that site did not exist the day it was built,
// it was published four months later, so a check made only at build time
// would have blessed that version as good.
//
//   - Who knows what is vulnerable is npm's advisory database, not a table
//     here: a table written by hand ages exactly like that dependency did.
//   - Only critical wakes anyone. Measured in V1: 3 critical and 94 high
//     across 33 sites; with high, the warning is noise, and whoever gets
//     thirty a day stops reading the one that matters. The rest is kept
//     for whoever goes to look.
//   - And only what is new: an advisory's key carries its range, so when
//     npm widens the affected range it is news again.
//   - Never `npm audit fix`. This looks, it does not touch: a site's folder
//     belongs to its project, and a dependency updated behind its back is
//     a deploy nobody asked for.
package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
)

// Site is what the auditor needs.
type Site struct {
	Domain string
	Dir    string
}

// Options configure an Auditor.
type Options struct {
	DataDir string
	Sites   func() []Site
	Bus     *events.Bus
	// NPM is the npm binary to run; "" means not available.
	NPM func() string
	// Every is the period; zero is a day.
	Every time.Duration
	Now   func() time.Time
}

// Advisory is one vulnerable package as npm reports it.
type Advisory struct {
	Name     string `json:"name"`
	Severity string `json:"severity"`
	Range    string `json:"range"`
	Title    string `json:"title,omitempty"`
	URL      string `json:"url,omitempty"`
	// Key is what makes it "the same advisory" from one day to the next.
	Key string `json:"key"`
}

// Report is one site's last audit.
type Report struct {
	Domain string         `json:"domain"`
	At     time.Time      `json:"at"`
	Counts map[string]int `json:"counts"`
	// Critical and High are listed; the rest are only counted.
	Critical []Advisory `json:"critical"`
	High     []Advisory `json:"high"`
	// Skipped says why a site was not audited (no lockfile).
	Skipped string `json:"skipped,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Auditor runs the audits.
type Auditor struct {
	opts Options
	mu   sync.Mutex // one audit at a time: npm is heavy
}

// New builds an auditor.
func New(opts Options) *Auditor {
	if opts.Every == 0 {
		opts.Every = 24 * time.Hour
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Bus == nil {
		opts.Bus = events.New("")
	}
	return &Auditor{opts: opts}
}

func (a *Auditor) path(domain string) string {
	return filepath.Join(a.opts.DataDir, "audit", domain+".json")
}

// Reports is every site's last report, by domain.
func (a *Auditor) Reports() []Report {
	var out []Report
	for _, s := range a.opts.Sites() {
		var r Report
		if err := fsx.ReadJSON(a.path(s.Domain), &r); err == nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}

// Report is one site's last report, or ok false.
func (a *Auditor) Report(domain string) (Report, bool) {
	var r Report
	err := fsx.ReadJSON(a.path(domain), &r)
	return r, err == nil
}

// Check audits one site now and records it.
func (a *Auditor) Check(ctx context.Context, s Site) Report {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := Report{Domain: s.Domain, At: a.opts.Now().UTC(), Counts: map[string]int{}, Critical: []Advisory{}, High: []Advisory{}}
	switch {
	case !fsx.Exists(filepath.Join(s.Dir, "package.json")):
		return r // not a Node project: nothing to say, nothing recorded
	case !fsx.Exists(filepath.Join(s.Dir, "package-lock.json")):
		r.Skipped = "no package-lock.json: npm audit needs one to know what is installed"
	default:
		npm := ""
		if a.opts.NPM != nil {
			npm = a.opts.NPM()
		}
		if npm == "" {
			r.Error = "npm is not available"
		} else if err := a.run(ctx, npm, s, &r); err != nil {
			r.Error = err.Error()
		}
	}
	var prev Report
	_ = fsx.ReadJSON(a.path(s.Domain), &prev)
	_ = fsx.WriteJSON(a.path(s.Domain), r, 0o644)
	a.announce(prev, r)
	return r
}

func (a *Auditor) run(ctx context.Context, npm string, s Site, r *Report) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	// --package-lock-only: the lockfile says what is installed, so the
	// audit needs no node_modules and installs nothing.
	cmd := exec.CommandContext(ctx, npm, "audit", "--json", "--package-lock-only")
	cmd.Dir = s.Dir
	cache := filepath.Join(a.opts.DataDir, "audit", ".npm")
	cmd.Env = append(npmEnv(), "npm_config_cache="+cache, "npm_config_update_notifier=false", "npm_config_fund=false")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	// npm audit exits 1 when it finds anything; the JSON is the answer.
	if out.Len() == 0 {
		if err != nil {
			msg := strings.TrimSpace(errb.String())
			if msg == "" {
				msg = err.Error()
			}
			return errors.New(msg)
		}
		return errors.New("npm audit printed nothing")
	}
	return parse(out.Bytes(), r)
}

// npmEnv is what npm gets from the supervisor's environment: a short
// allow-list, never the whole of it. npm runs in the site's folder and
// reads the site's own .npmrc, which expands ${VAR} from the environment
// into any setting, the registry's address included; with os.Environ()
// a site (a docker one, or a conversation allowed to edit its folder)
// could have the daily audit send the supervisor's MIDDLEWARE_SECRET_KEY,
// the key to every module's credentials, to an address of its choosing.
// A site's process never gets these (runtime.buildEnv); the audit run on
// its folder must not either.
func npmEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "HOME", "LANG", "LC_ALL", "TZ", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// parse reads npm audit's JSON (npm 7 and later).
func parse(data []byte, r *Report) error {
	var doc struct {
		Error *struct {
			Summary string `json:"summary"`
		} `json:"error"`
		Vulnerabilities map[string]struct {
			Name     string            `json:"name"`
			Severity string            `json:"severity"`
			Range    string            `json:"range"`
			Via      []json.RawMessage `json:"via"`
		} `json:"vulnerabilities"`
		Metadata struct {
			Vulnerabilities map[string]int `json:"vulnerabilities"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("npm audit: %w", err)
	}
	if doc.Error != nil {
		return errors.New("npm audit: " + doc.Error.Summary)
	}
	for k, v := range doc.Metadata.Vulnerabilities {
		if k != "total" {
			r.Counts[k] = v
		}
	}
	for name, v := range doc.Vulnerabilities {
		if v.Severity != "critical" && v.Severity != "high" {
			continue
		}
		adv := Advisory{Name: name, Severity: v.Severity, Range: v.Range}
		// "via" is either the advisories themselves or the names of the
		// packages that bring them; the first advisory names it.
		for _, raw := range v.Via {
			var via struct {
				Title  string `json:"title"`
				URL    string `json:"url"`
				Source any    `json:"source"`
			}
			if json.Unmarshal(raw, &via) == nil && via.Title != "" {
				adv.Title = via.Title
				// The report comes from whichever registry the site's own
				// .npmrc names, and the panel turns this into a link: only
				// a web address gets through, never a javascript: one.
				if strings.HasPrefix(via.URL, "https://") || strings.HasPrefix(via.URL, "http://") {
					adv.URL = via.URL
				}
				break
			}
		}
		adv.Key = name + "@" + v.Range + "#" + adv.URL
		if v.Severity == "critical" {
			r.Critical = append(r.Critical, adv)
		} else {
			r.High = append(r.High, adv)
		}
	}
	sort.Slice(r.Critical, func(i, j int) bool { return r.Critical[i].Key < r.Critical[j].Key })
	sort.Slice(r.High, func(i, j int) bool { return r.High[i].Key < r.High[j].Key })
	return nil
}

// announce emits deps.critical for criticals the previous audit did not
// have, which is the event the operator's phone hears.
func (a *Auditor) announce(prev, r Report) {
	known := map[string]bool{}
	for _, c := range prev.Critical {
		known[c.Key] = true
	}
	var fresh []string
	for _, c := range r.Critical {
		if !known[c.Key] {
			fresh = append(fresh, c.Name+" "+c.Range)
		}
	}
	if len(fresh) == 0 {
		return
	}
	a.opts.Bus.Emit(events.Event{Type: "deps.critical", Subject: r.Domain, Actor: "engine", Level: "error",
		Message: fmt.Sprintf("%d new critical: %s", len(fresh), strings.Join(fresh, ", ")),
		Data:    map[string]any{"new": fresh}})
}

// CheckAll audits every site, one after another.
func (a *Auditor) CheckAll(ctx context.Context) {
	for _, s := range a.opts.Sites() {
		if ctx.Err() != nil {
			return
		}
		a.Check(ctx, s)
	}
	a.opts.Bus.Emit(events.Event{Type: "deps.audited", Actor: "engine", Message: "dependency audit done"})
}

// Run audits a few minutes after start and then every period.
func (a *Auditor) Run(ctx context.Context) {
	t := time.NewTimer(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.CheckAll(ctx)
		t.Reset(a.opts.Every)
	}
}
