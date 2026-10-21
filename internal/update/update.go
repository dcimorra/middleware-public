// Package update keeps this program itself current. It is off unless the
// operator turns it on (settings update.auto), and a button does the same
// at any moment. The supervisor only finds out which release is newer;
// installing it is the privileged helper's (update.install), which
// downloads from the fixed repository, checks the sha256 and refuses
// anything not newer than what runs. Then both restart: the helper by
// ending, the supervisor with SIGTERM to itself, and the sites, which
// outlive it, are re-adopted.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/release"
)

// DefaultEvery is how often a new release is looked for when the
// operator did not say.
const DefaultEvery = time.Hour

// fresh is how long a check is reused before asking GitHub again.
const fresh = time.Hour

// Options configure an Updater.
type Options struct {
	Current string
	Bus     *events.Bus
	// Auto says whether a release found is installed by itself.
	Auto func() bool
	// Every is how often to look; nil or zero is DefaultEvery. Read before
	// every wait, so a change applies without a restart.
	Every func() time.Duration
	// Install asks the helper to install version. Helper says whether
	// there is one to ask; without, the panel says to use the script.
	Install func(ctx context.Context, version string) error
	Helper  func() bool
	// Restart ends the supervisor so systemd starts the new binary.
	Restart func()
	HTTP    *http.Client
	Arch    string
	Now     func() time.Time
}

// Status is what the System screen shows.
type Status struct {
	Current   string    `json:"current"`
	Latest    string    `json:"latest,omitempty"`
	Available bool      `json:"available"`
	CheckedAt time.Time `json:"checked_at,omitzero"`
	Error     string    `json:"error,omitempty"`
	Auto      bool      `json:"auto"`
	// EveryMinutes is the interval the checks run at; NextCheck when the
	// next one is due.
	EveryMinutes int       `json:"every_minutes"`
	NextCheck    time.Time `json:"next_check,omitzero"`
	Installing   bool      `json:"installing,omitempty"`
	// CanInstall is false without the helper (a container, an install
	// with --no-helper) or on a build that is not a release.
	CanInstall bool `json:"can_install"`
	// Why is the reason it cannot, as a code the panel translates:
	// "not_release" or "no_helper".
	Why string `json:"why,omitempty"`
}

// The reasons an update cannot be installed from here.
const (
	WhyNotRelease = "not_release"
	WhyNoHelper   = "no_helper"
)

// whyText says the reason in a sentence, for the API's errors.
var whyText = map[string]string{
	WhyNotRelease: "this build is not a release",
	WhyNoHelper:   "there is no privileged helper: update with the install script",
}

// Updater checks and installs.
type Updater struct {
	opts Options
	mu   sync.Mutex
	st   Status
	// told is the last release announced as waiting for the button, so it
	// is announced once and not at every check.
	told string
	next time.Time
}

// New builds an updater.
func New(opts Options) *Updater {
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{Timeout: 20 * time.Second}
	}
	if opts.Arch == "" {
		opts.Arch = runtime.GOARCH
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Bus == nil {
		opts.Bus = events.New("")
	}
	return &Updater{opts: opts, st: Status{Current: opts.Current}}
}

// Status returns the last check.
func (u *Updater) Status() Status {
	u.mu.Lock()
	st := u.st
	u.mu.Unlock()
	st.Auto = u.opts.Auto != nil && u.opts.Auto()
	st.EveryMinutes = int(u.every() / time.Minute)
	u.mu.Lock()
	st.NextCheck = u.next
	u.mu.Unlock()
	st.CanInstall, st.Why = true, ""
	switch {
	case !release.Valid(u.opts.Current):
		st.CanInstall, st.Why = false, WhyNotRelease
	case u.opts.Install == nil || u.opts.Helper != nil && !u.opts.Helper():
		st.CanInstall, st.Why = false, WhyNoHelper
	}
	return st
}

// Check asks GitHub for the newest release, reusing an hour-old answer
// unless force.
func (u *Updater) Check(ctx context.Context, force bool) Status {
	u.mu.Lock()
	recent := !u.st.CheckedAt.IsZero() && u.opts.Now().Sub(u.st.CheckedAt) < fresh && u.st.Error == ""
	u.mu.Unlock()
	if recent && !force {
		return u.Status()
	}
	latest, err := u.latest(ctx)
	u.mu.Lock()
	u.st.CheckedAt = u.opts.Now()
	u.st.Error = ""
	if err != nil {
		u.st.Error = err.Error()
	} else {
		u.st.Latest = latest
		u.st.Available = release.Newer(latest, u.opts.Current)
	}
	u.mu.Unlock()
	return u.Status()
}

// latest is the newest published release with a binary for this machine.
// A pre-release is offered only to an installation running one: who runs
// a stable release stays on stable ones.
func (u *Updater) latest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, release.APIBase+"/releases?per_page=30", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.opts.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github answered %s", resp.Status)
	}
	var list []struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return "", err
	}
	wantPre := isPre(u.opts.Current)
	asset := release.Asset(u.opts.Arch)
	best := ""
	for _, r := range list {
		if r.Draft || !release.Valid(r.Tag) || (isPre(r.Tag) || r.Prerelease) && !wantPre {
			continue
		}
		has := false
		for _, a := range r.Assets {
			if a.Name == asset {
				has = true
			}
		}
		if has && (best == "" || release.Newer(r.Tag, best)) {
			best = r.Tag
		}
	}
	if best == "" {
		return "", errors.New("no published release has a binary for " + u.opts.Arch)
	}
	return best, nil
}

func isPre(tag string) bool { return release.Valid(tag) && strings.Contains(tag, "-") }

// ErrNothingNewer is an install asked for with nothing to install.
var ErrNothingNewer = errors.New("this is the newest release")

// Install installs the newest release now and restarts. It answers once
// the helper has replaced the binary; the restart follows a moment later.
func (u *Updater) Install(ctx context.Context, actor string) (string, error) {
	st := u.Check(ctx, true)
	if !st.CanInstall {
		return "", errors.New(whyText[st.Why])
	}
	if st.Error != "" {
		return "", errors.New(st.Error)
	}
	if !st.Available {
		return "", ErrNothingNewer
	}
	u.mu.Lock()
	if u.st.Installing {
		u.mu.Unlock()
		return "", errors.New("an update is already being installed")
	}
	u.st.Installing = true
	u.mu.Unlock()
	u.opts.Bus.Emit(events.Event{Type: "update.started", Actor: actor, Message: u.opts.Current + " → " + st.Latest})
	err := u.opts.Install(ctx, st.Latest)
	u.mu.Lock()
	u.st.Installing = false
	u.mu.Unlock()
	if err != nil {
		u.opts.Bus.Emit(events.Event{Type: "update.failed", Actor: actor, Level: "error", Message: err.Error()})
		return "", err
	}
	u.opts.Bus.Emit(events.Event{Type: "update.installed", Actor: actor, Message: u.opts.Current + " → " + st.Latest + "; restarting"})
	if u.opts.Restart != nil {
		go func() {
			time.Sleep(time.Second)
			u.opts.Restart()
		}()
	}
	return st.Latest, nil
}

func (u *Updater) every() time.Duration {
	if u.opts.Every != nil {
		if d := u.opts.Every(); d >= time.Minute {
			return d
		}
	}
	return DefaultEvery
}

// Run looks for a new release every interval, the first time two minutes
// after starting. One found is installed when Auto is on; otherwise it is
// announced, once, and waits for the button.
func (u *Updater) Run(ctx context.Context) {
	last := u.opts.Now().Add(-u.every() + 2*time.Minute)
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		due := last.Add(u.every())
		u.mu.Lock()
		u.next = due
		u.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// Every tick, not one long sleep: an interval shortened in the
		// panel applies now, not after the old one runs out.
		if u.opts.Now().Before(due) {
			continue
		}
		last = u.opts.Now()
		u.tick(ctx)
	}
}

func (u *Updater) tick(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	st := u.Check(cctx, true)
	cancel()
	if !st.Available {
		return
	}
	if st.Auto && st.CanInstall {
		ictx, cancel := context.WithTimeout(ctx, 11*time.Minute)
		defer cancel()
		_, _ = u.Install(ictx, "engine")
		return
	}
	u.mu.Lock()
	told := u.told == st.Latest
	u.told = st.Latest
	u.mu.Unlock()
	if !told {
		u.opts.Bus.Emit(events.Event{Type: "update.available", Actor: "engine", Level: "warn", Message: st.Latest + " is available (running " + st.Current + "); install it from System"})
	}
}
