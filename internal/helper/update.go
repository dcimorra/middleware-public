package helper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/release"
)

// UpdateArgs is the input of update.install: the release's name and
// nothing else. Where it comes from is fixed (release.Repo), and it must
// be newer than what runs, so a supervisor that was broken into can
// neither install a binary of its own nor go back to an older, weaker one.
type UpdateArgs struct {
	Version string `json:"version"`
}

// UpdateResult says what was replaced.
type UpdateResult struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Binary string `json:"binary"`
}

var updateHTTP = &http.Client{Timeout: 10 * time.Minute}

// binaryPath is the executable to replace: this one, which is also the
// supervisor's (one binary, two units).
func (s *Server) binaryPath() (string, error) {
	if s.Binary != "" {
		return s.Binary, nil
	}
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

func (s *Server) updateInstall(ctx context.Context, raw json.RawMessage) (any, error) {
	var a UpdateArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if !release.Valid(a.Version) {
		return nil, fmt.Errorf("%w: %q is not a release name", errBadArgument, a.Version)
	}
	if !release.Newer(a.Version, s.Build) {
		return nil, fmt.Errorf("%s is not newer than the running %s", a.Version, s.Build)
	}
	bin, err := s.binaryPath()
	if err != nil {
		return nil, err
	}
	// One install at a time, and none while the record is being read or
	// written by a confirmation or a rollback.
	s.updMu.Lock()
	defer s.updMu.Unlock()
	st := s.readUpdateState(bin)
	if p := st.Pending; p != nil && p.To == s.Build {
		// What runs is itself on trial: installing over it would make the
		// unconfirmed binary the one a rollback goes back to.
		return nil, fmt.Errorf("the update to %s is not confirmed yet", p.To)
	}
	if st.Skipped != "" && !release.Newer(a.Version, st.Skipped) {
		return nil, fmt.Errorf("%s was rolled back on this machine: it never said it was serving; a release newer than %s will be installed, not it", a.Version, st.Skipped)
	}
	arch := s.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}
	asset := release.Asset(arch)
	sum, err := fetchSum(ctx, release.URL(a.Version, asset+".sha256"))
	if err != nil {
		return nil, err
	}
	// Next to the target, so the rename is atomic and a half-written
	// binary is never what the next start runs.
	dir := filepath.Dir(bin)
	tmp, err := os.CreateTemp(dir, ".middleware-update-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	got, err := download(ctx, release.URL(a.Version, asset), tmp)
	if err == nil {
		// Without it the rename can reach the disk before the data does,
		// and a power cut leaves an empty file where the binary was: both
		// units then fail to start and nothing is left to roll back with.
		err = syncFile(tmp)
	}
	tmp.Close()
	if err != nil {
		return nil, err
	}
	if got != sum {
		return nil, fmt.Errorf("%s: sha256 %s does not match the published %s", asset, got, sum)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return nil, err
	}
	// It must run here and be the release asked for: an arm64 binary on
	// amd64, or a mislabelled asset, is found now and not at the restart.
	vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, tmp.Name(), "version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "middleware "+a.Version {
		return nil, fmt.Errorf("the downloaded binary does not say it is %s: %v %s", a.Version, err, strings.TrimSpace(string(out)))
	}
	// Saying its version is all a binary can prove before it runs. That
	// it serves is proved by the new supervisor (update.confirm); until
	// then the one running now is kept, and the record says by when the
	// proof is due.
	if err := keepPrevious(bin); err != nil {
		return nil, fmt.Errorf("keep %s: %w", bin+PrevSuffix, err)
	}
	before := st
	st.Pending = &PendingUpdate{From: s.Build, To: a.Version, Deadline: time.Now().Add(s.updateGrace())}
	if err := writeUpdateState(bin, st); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), bin); err != nil {
		_ = writeUpdateState(bin, before)
		return nil, err
	}
	if err := syncDir(dir); err != nil {
		return nil, err
	}
	// The helper is running the old binary: it ends after answering and
	// systemd (Restart=always) starts the new one, whose WatchUpdate
	// holds the record.
	go func() {
		time.Sleep(time.Second)
		s.Log.Printf("updated to %s; restarting", a.Version)
		s.exit()
	}()
	return UpdateResult{From: s.Build, To: a.Version, Binary: bin}, nil
}

func (s *Server) exit() {
	if s.Exit != nil {
		s.Exit()
		return
	}
	os.Exit(0)
}

func fetchSum(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := updateHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(body))
	if len(f) == 0 || len(f[0]) != 64 {
		return "", errors.New("the published sha256 does not read")
	}
	return strings.ToLower(f[0]), nil
}

func download(ctx context.Context, url string, w io.Writer) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := updateHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", url, resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, 512<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// UpdateInstall asks the helper to install a release; it downloads it
// itself, so the call takes as long as the download.
func (c *Client) UpdateInstall(ctx context.Context, version string) (UpdateResult, error) {
	var r UpdateResult
	long := &Client{path: c.path, timeout: 10 * time.Minute}
	err := long.Call(ctx, OpUpdateInstall, UpdateArgs{Version: version}, &r)
	return r, err
}

// An update is on trial until the supervisor it started says it serves.
//
// Before this, `version` printing the right name was the whole check and
// the rename threw the old binary away. A release that panicked in
// `serve` then crash-looped the supervisor, which is the proxy for every
// site, on every server with automatic updates at once, and the helper
// ran the same new binary, so there was nothing left to go back with.
// Now update.install keeps the binary that ran as middleware.prev and
// writes a record {from, to, deadline}; the new supervisor, once its
// listener is open and the engine has been running a while, calls
// update.confirm and the record is cleared. The helper (the new binary
// too: it restarts after every install) watches the record from its own
// start, and when the deadline passes unconfirmed, or the supervisor
// keeps restarting, or systemd gave up on it, puts middleware.prev back,
// restarts the supervisor and ends so systemd starts it on the old binary.
//
// Going back is the helper's own decision, from its own record: no
// operation asks for an older version, so update.install's rule (only
// newer, only from the fixed repository) is the whole of what the socket
// can install. A supervisor that was broken into can at most withhold its
// confirmation and get the release that ran minutes ago back.
//
// The version rolled back from is remembered as skipped, and
// update.install refuses it and anything older until a newer one is
// asked for; otherwise automatic updates would install it again at the
// next check, and the machine would go dark every hour.
//
// The record and middleware.prev live next to the binary: that folder is
// root's, the supervisor cannot write it, and it is the one writable
// place every helper unit ever rendered has (ReadWritePaths), which
// matters because an update replaces the binary and never the units.

// PrevSuffix names the binary that ran before the last update, kept next
// to it until the next one.
const PrevSuffix = ".prev"

// StateSuffix names the update record, next to the binary.
const StateSuffix = ".update.json"

// supervisorUnit is the unit a rollback restarts.
const supervisorUnit = "middleware.service"

// DefaultUpdateGrace is how long a new supervisor has to confirm. It
// starts at the install, and the supervisor needs a few seconds to be
// restarted and half a minute of serving before it confirms; five minutes
// leaves room for a slow machine and is still short enough that a dark
// server is not one for long.
const DefaultUpdateGrace = 5 * time.Minute

// crashRestarts is how many restarts of the supervisor during the trial
// count as a crash loop. Restart=always with RestartSec=2 gets there in
// under ten seconds; the restart that follows the install itself is one,
// and an operator pressing Restart once more is two.
const crashRestarts = 3

// PendingUpdate is an update not yet confirmed.
type PendingUpdate struct {
	From     string    `json:"from"`
	To       string    `json:"to"`
	Deadline time.Time `json:"deadline"`
}

// Rollback says what went back and why.
type Rollback struct {
	From string    `json:"from"`
	To   string    `json:"to"`
	At   time.Time `json:"at"`
	Why  string    `json:"why"`
}

// UpdateState is the record. Rollback is kept until a confirmation has
// reported it once, so the supervisor says it happened one time and not
// at every start.
type UpdateState struct {
	Pending  *PendingUpdate `json:"pending,omitempty"`
	Skipped  string         `json:"skipped,omitempty"`
	Rollback *Rollback      `json:"rollback,omitempty"`
}

// ConfirmArgs is the input of update.confirm: the version the caller runs.
type ConfirmArgs struct {
	Version string `json:"version"`
}

// ConfirmResult says whether an update was on trial and what is skipped.
type ConfirmResult struct {
	Confirmed bool      `json:"confirmed"`
	Skipped   string    `json:"skipped,omitempty"`
	Rollback  *Rollback `json:"rollback,omitempty"`
}

func (s *Server) updateGrace() time.Duration {
	if s.UpdateGrace > 0 {
		return s.UpdateGrace
	}
	return DefaultUpdateGrace
}

func (s *Server) watchEvery() time.Duration {
	if s.WatchEvery > 0 {
		return s.WatchEvery
	}
	return 10 * time.Second
}

// readUpdateState is the record; a missing or unreadable one is none. An
// unreadable record is not trusted with a rollback: going back on a guess
// would be worse than a missed one, which the operator can still undo by
// hand from middleware.prev.
func (s *Server) readUpdateState(bin string) UpdateState {
	var st UpdateState
	if err := fsx.ReadJSON(bin+StateSuffix, &st); err != nil && !fsx.IsNotExist(err) {
		s.Log.Printf("update record: %v", err)
		return UpdateState{}
	}
	return st
}

func writeUpdateState(bin string, st UpdateState) error {
	path := bin + StateSuffix
	if st.Pending == nil && st.Skipped == "" && st.Rollback == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if err := fsx.WriteJSON(path, st, 0o644); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncFile is a variable so a test can see what was synced and when.
var syncFile = func(f *os.File) error { return f.Sync() }

// syncDir makes the renames in dir durable: a file's own fsync says
// nothing of the directory entry that names it.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return syncFile(d)
}

// keepPrevious copies bin to bin.prev, synced. A copy and not a hard
// link: a link shares the inode, and an operator's `cp` over the binary
// (which writes in place) would change the kept one with it.
func keepPrevious(bin string) error {
	src, err := os.Open(bin)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(bin), ".middleware-prev-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, src)
	if err == nil {
		err = syncFile(tmp)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), bin+PrevSuffix)
}

func (s *Server) updateConfirm(_ context.Context, raw json.RawMessage) (any, error) {
	var a ConfirmArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if !release.Valid(a.Version) {
		return nil, fmt.Errorf("%w: %q is not a release name", errBadArgument, a.Version)
	}
	bin, err := s.binaryPath()
	if err != nil {
		return nil, err
	}
	s.updMu.Lock()
	defer s.updMu.Unlock()
	st := s.readUpdateState(bin)
	res := ConfirmResult{Skipped: st.Skipped, Rollback: st.Rollback}
	changed := st.Rollback != nil
	st.Rollback = nil
	if p := st.Pending; p != nil {
		// Only the version on trial confirms it: the old supervisor still
		// shutting down, or the one a rollback brought back, must not.
		if a.Version != p.To {
			return nil, fmt.Errorf("the update on trial is to %s, not %s", p.To, a.Version)
		}
		st.Pending = nil
		res.Confirmed = true
		changed = true
		s.Log.Printf("update to %s confirmed", a.Version)
	}
	if st.Skipped != "" && release.Newer(a.Version, st.Skipped) {
		st.Skipped, res.Skipped = "", ""
		changed = true
	}
	if changed {
		if err := writeUpdateState(bin, st); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// WatchUpdate holds an update on trial: it returns at once when there is
// none, when it is confirmed, and after a rollback (which also ends the
// helper). It runs from the helper's start because the helper is
// restarted by every install, and so is always the new binary watching
// the supervisor the same install restarted. A helper that crashes
// before this would also have failed `version`, which the install runs.
// A machine rebooted during the trial rolls back at once if the deadline
// passed while it was down: a skipped good release costs one release
// cycle, a dark server costs every site.
func (s *Server) WatchUpdate(ctx context.Context) {
	s.init()
	bin, err := s.binaryPath()
	if err != nil {
		return
	}
	s.updMu.Lock()
	st := s.readUpdateState(bin)
	p := st.Pending
	if p != nil && p.To != s.Build {
		// The record names a binary that is not this one: the install
		// failed after writing it, a rollback was cut short after the
		// rename, or someone put another binary in place by hand. None of
		// them is an update of ours to judge.
		s.Log.Printf("update record to %s does not match this binary (%s); dropped", p.To, s.Build)
		st.Pending = nil
		_ = writeUpdateState(bin, st)
		p = nil
	}
	s.updMu.Unlock()
	if p == nil {
		return
	}
	s.Log.Printf("update %s → %s on trial until %s", p.From, p.To, p.Deadline.Format(time.RFC3339))
	base, _, _ := s.supervisorState(ctx)
	t := time.NewTicker(s.watchEvery())
	defer t.Stop()
	for {
		why := ""
		if !time.Now().Before(p.Deadline) {
			why = "the supervisor did not confirm it was serving by " + p.Deadline.Format(time.RFC3339)
		} else if n, active, err := s.supervisorState(ctx); err == nil {
			switch {
			case active == "failed":
				why = "systemd gave up restarting the supervisor"
			case n-base >= crashRestarts:
				why = fmt.Sprintf("the supervisor restarted %d times", n-base)
			}
		}
		if why != "" {
			if s.rollback(ctx, bin, *p, why) {
				s.exit()
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.updMu.Lock()
		cur := s.readUpdateState(bin).Pending
		s.updMu.Unlock()
		if cur == nil || *cur != *p {
			return
		}
	}
}

// supervisorState is the supervisor unit's restart count and state.
func (s *Server) supervisorState(ctx context.Context) (int, string, error) {
	out, err := s.Exec.Run(ctx, "systemctl", []string{"show", supervisorUnit, "--property=NRestarts,ActiveState"}, "")
	if err != nil {
		return 0, "", err
	}
	n, active := 0, ""
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "NRestarts":
			n, _ = strconv.Atoi(v)
		case "ActiveState":
			active = v
		}
	}
	return n, active, nil
}

// rollback puts middleware.prev back and restarts the supervisor on it.
// It reports whether the binary went back, so the caller ends the helper
// only then. The order makes a crash anywhere in between safe: the skip
// is written first (a crash leaves the record on trial and past its
// deadline, and the next start rolls back again), then the rename (after
// which the record names a binary that is not the one running, and is
// dropped with the skip kept), then the record is closed.
func (s *Server) rollback(ctx context.Context, bin string, p PendingUpdate, why string) bool {
	s.updMu.Lock()
	defer s.updMu.Unlock()
	st := s.readUpdateState(bin)
	if st.Pending == nil || *st.Pending != p {
		// Confirmed while this was deciding.
		return false
	}
	s.Log.Printf("rolling back %s → %s: %s", p.To, p.From, why)
	prev := bin + PrevSuffix
	if _, err := os.Stat(prev); err != nil {
		s.Log.Printf("cannot roll back: %v", err)
		return false
	}
	st.Skipped = p.To
	if err := writeUpdateState(bin, st); err != nil {
		s.Log.Printf("cannot roll back: %v", err)
		return false
	}
	if err := os.Rename(prev, bin); err != nil {
		s.Log.Printf("cannot roll back: %v", err)
		return false
	}
	if err := syncDir(filepath.Dir(bin)); err != nil {
		s.Log.Printf("roll back: %v", err)
	}
	st.Pending = nil
	st.Rollback = &Rollback{From: p.To, To: p.From, At: time.Now(), Why: why}
	if err := writeUpdateState(bin, st); err != nil {
		s.Log.Printf("roll back: %v", err)
	}
	// reset-failed first: a unit that hit its start limit refuses a
	// restart until it is cleared, and that is the case that most needs
	// one. --no-block because stopping a supervisor that hangs takes its
	// TimeoutStopSec, and the helper has nothing to wait for.
	_, _ = s.Exec.Run(ctx, "systemctl", []string{"reset-failed", supervisorUnit}, "")
	if _, err := s.Exec.Run(ctx, "systemctl", []string{"restart", "--no-block", supervisorUnit}, ""); err != nil {
		s.Log.Printf("roll back: restart %s: %v", supervisorUnit, err)
	}
	s.Log.Printf("rolled back to %s; %s is skipped until a newer release; restarting", p.From, p.To)
	return true
}

// UpdateConfirm tells the helper that version is serving. It answers what
// is skipped and, once, the rollback that happened.
func (c *Client) UpdateConfirm(ctx context.Context, version string) (ConfirmResult, error) {
	var r ConfirmResult
	err := c.Call(ctx, OpUpdateConfirm, ConfirmArgs{Version: version}, &r)
	return r, err
}
