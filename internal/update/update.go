// Package update keeps this program itself current. It is off unless the
// operator turns it on (settings update.auto), and a button does the same
// at any moment. The supervisor only finds out which release is newer;
// installing it is the privileged helper's (update.install), which
// downloads from the fixed repository, checks the sha256 and refuses
// anything not newer than what runs. Then both restart: the helper by
// ending, the supervisor with SIGTERM to itself, and the sites, which
// outlive it, are re-adopted.
//
// The new release is on trial until the new supervisor says it serves
// (ConfirmWhenServing, the helper's update.confirm); one that does not
// say so in time is rolled back by the helper on its own and skipped
// until a newer release is out. See internal/helper/update.go.
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

// DeferRetry is how soon an automatic install that waited for Busy is
// tried again.
const DeferRetry = time.Minute

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
	// Busy says whether something a restart would cut is running (a move
	// between nodes). An automatic install waits while it does; Install,
	// the operator's button, does not.
	Busy func() bool
	// Confirm tells the helper that Current is serving (update.confirm).
	// nil is an installation without the helper, where nothing is ever
	// on trial.
	Confirm func(ctx context.Context, version string) (Confirmation, error)
	// Settle is how long the supervisor serves before it confirms, and
	// Probe how it proves it does, given the listener's address; zero and
	// nil are the defaults (tests shorten and replace them).
	Settle time.Duration
	Probe  func(ctx context.Context, listen string) error
	HTTP   *http.Client
	Arch   string
	Now    func() time.Time
}

// Confirmation is the helper's answer to a confirmation.
type Confirmation struct {
	// Confirmed is true when Current was on trial and no longer is.
	Confirmed bool
	// Skipped is a release the helper rolled back from and will not
	// install; RolledBack and Why are said once, at the first start
	// after the rollback.
	Skipped    string
	RolledBack string
	Why        string
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
	// Skipped is a release this machine rolled back from: it did not say
	// it was serving after it was installed. It is not offered, and the
	// helper refuses it, until a newer one is out.
	Skipped string `json:"skipped,omitempty"`
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
	// deferred is the last release announced as waiting for Busy, for the
	// same reason.
	deferred string
	next     time.Time
	// poll is how often Run looks at the clock; zero is 15 seconds.
	poll time.Duration
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
		u.st.Available = u.offered(latest)
	}
	u.mu.Unlock()
	return u.Status()
}

// offered says whether latest is one to install: newer than what runs,
// and newer than a release this machine rolled back from. Called with
// u.mu held.
func (u *Updater) offered(latest string) bool {
	return release.Newer(latest, u.opts.Current) && (u.st.Skipped == "" || release.Newer(latest, u.st.Skipped))
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
	poll := u.poll
	if poll == 0 {
		poll = 15 * time.Second
	}
	t := time.NewTicker(poll)
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
		if u.tick(ctx) {
			last = last.Add(-u.every() + DeferRetry)
		}
	}
}

// tick looks once, and reports whether an install was deferred.
func (u *Updater) tick(ctx context.Context) bool {
	// A retry after a deferral reuses the last answer: a move can last an
	// hour, and asking GitHub every minute of it would spend the hourly
	// allowance of an address without a token.
	u.mu.Lock()
	force := u.deferred == ""
	u.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	st := u.Check(cctx, force)
	cancel()
	deferring := st.Available && st.Auto && st.CanInstall && u.opts.Busy != nil && u.opts.Busy()
	if !deferring {
		u.mu.Lock()
		u.deferred = ""
		u.mu.Unlock()
	}
	if !st.Available {
		return false
	}
	if st.Auto && st.CanInstall {
		// The install ends in a restart of the supervisor, and a move
		// between nodes is the supervisor copying a site: restarted in
		// the middle, the move is cut and the site is left half here and
		// half there. An automatic install waits for it, looked at again
		// in DeferRetry rather than a whole interval later; the operator's
		// button does not wait, the operator having chosen.
		if deferring {
			u.mu.Lock()
			told := u.deferred == st.Latest
			u.deferred = st.Latest
			u.mu.Unlock()
			if !told {
				u.opts.Bus.Emit(events.Event{Type: "update.deferred", Actor: "engine", Message: st.Latest + " waits for the moves between nodes to end before it installs"})
			}
			return true
		}
		ictx, cancel := context.WithTimeout(ctx, 11*time.Minute)
		defer cancel()
		_, _ = u.Install(ictx, "engine")
		return false
	}
	u.mu.Lock()
	told := u.told == st.Latest
	u.told = st.Latest
	u.mu.Unlock()
	if !told {
		u.opts.Bus.Emit(events.Event{Type: "update.available", Actor: "engine", Level: "warn", Message: st.Latest + " is available (running " + st.Current + "); install it from System"})
	}
	return false
}

// DefaultSettle is how long the supervisor serves before it confirms an
// update. The engine adopts the running sites and then reconciles every
// two seconds: half a minute is a dozen passes, enough for a release that
// panics in the engine, the proxy or a hook's loop to have done so, and
// well inside the helper's five minutes.
const DefaultSettle = 30 * time.Second

// confirmFor is how long a confirmation is retried: the helper's grace.
// Past it the helper has decided, and asking again is noise.
const confirmFor = 5 * time.Minute

// startedEvent is what app.Run emits once the loopback listener is open,
// with its address in Data["listen"].
const startedEvent = "middleware.started"

// ConfirmWhenServing confirms Current to the helper once this supervisor
// serves: its listener open (startedEvent, emitted after it is), Settle
// spent running, and a request answered through that listener. It
// returns at once and does the rest in a goroutine. A release that never
// gets there never confirms, and the helper puts the previous one back.
//
// Every start confirms, not only the one after an update: the supervisor
// cannot know whether it is on trial (the helper's record is root's), and
// with nothing on trial a confirmation only fetches what is skipped.
func (u *Updater) ConfirmWhenServing(ctx context.Context) {
	if u.opts.Confirm == nil || !release.Valid(u.opts.Current) {
		return
	}
	// Subscribed before Recent is read, so the event cannot fall between
	// the two: it was emitted already, or it is coming down ch.
	ch, stop := u.opts.Bus.Subscribe()
	go func() {
		defer stop()
		listen, ok := "", false
		for _, e := range u.opts.Bus.Recent() {
			if l, is := started(e); is {
				listen, ok = l, true
			}
		}
		for !ok {
			select {
			case <-ctx.Done():
				return
			case e := <-ch:
				listen, ok = started(e)
			}
		}
		stop()
		if !sleep(ctx, u.settle()) {
			return
		}
		probe := u.opts.Probe
		if probe == nil {
			probe = probeListener
		}
		// Retried: the helper restarts itself right after an install and
		// may not be back yet, and a request that failed once may not
		// fail twice. Neither is a reason to let a good release go.
		until := u.opts.Now().Add(confirmFor)
		var err error
		for {
			if err = probe(ctx, listen); err == nil {
				var c Confirmation
				if c, err = u.opts.Confirm(ctx, u.opts.Current); err == nil {
					u.confirmed(c)
					return
				}
			}
			if !u.opts.Now().Before(until) || !sleep(ctx, 5*time.Second) {
				break
			}
		}
		if ctx.Err() == nil && (u.opts.Helper == nil || u.opts.Helper()) {
			u.opts.Bus.Emit(events.Event{Type: "update.unconfirmed", Actor: "engine", Level: "warn", Message: u.opts.Current + " could not tell the helper it is serving: " + err.Error()})
		}
	}()
}

func started(e events.Event) (string, bool) {
	if e.Type != startedEvent {
		return "", false
	}
	l, _ := e.Data["listen"].(string)
	return l, true
}

func (u *Updater) settle() time.Duration {
	if u.opts.Settle > 0 {
		return u.opts.Settle
	}
	return DefaultSettle
}

func (u *Updater) confirmed(c Confirmation) {
	u.mu.Lock()
	u.st.Skipped = c.Skipped
	if u.st.Latest != "" {
		u.st.Available = u.offered(u.st.Latest)
	}
	u.mu.Unlock()
	if c.Confirmed {
		u.opts.Bus.Emit(events.Event{Type: "update.confirmed", Actor: "engine", Message: u.opts.Current + " is serving; the release before it is kept until the next update"})
	}
	if c.RolledBack != "" {
		u.opts.Bus.Emit(events.Event{Type: "update.rolled_back", Actor: "engine", Level: "error", Message: c.RolledBack + " was rolled back to " + u.opts.Current + ": " + c.Why + "; it is skipped until a newer release is out"})
	}
}

// probeHTTP has no proxy on purpose: MIDDLEWARE_LISTEN may be 0.0.0.0,
// which is not loopback to ProxyFromEnvironment, and a proxy answering
// would prove the proxy serves, not this process.
var probeHTTP = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}

// probeListener sends one request through the listener. Any answer will
// do, a 404 for an unknown name included: it went through the whole chain
// (WAF, router, proxy) and came back.
func probeListener(ctx context.Context, listen string) error {
	if listen == "" {
		return errors.New("the listener's address is not known")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listen+"/", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "middleware-update-confirm")
	resp, err := probeHTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// sleep waits d, or reports false when ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
