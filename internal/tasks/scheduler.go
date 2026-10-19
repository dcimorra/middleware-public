package tasks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/dcimorra/middleware-public/internal/cron"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/runtime"
)

// Options configure a Scheduler.
type Options struct {
	Root string
	Bus  *events.Bus
	// Tick is how often the folder is re-read; a minute by default.
	Tick time.Duration
	// Now is the clock; tests move it.
	Now func() time.Time
}

// Scheduler re-reads tasks/ every tick and runs what is due.
type Scheduler struct {
	opts Options
	mu   sync.Mutex
	// lastCheck is the minute the last tick covered: a task is due when
	// its next run after lastCheck is not after now. Starting it at boot
	// means a task missed while the supervisor was down is not replayed,
	// which is the crontab rule and the safe one for a backup.
	lastCheck time.Time
	states    map[string]*state
	wg        sync.WaitGroup
}

type state struct {
	running bool
	queued  bool
	lastErr string
}

// ErrRunning says the task is already running.
var ErrRunning = errors.New("task is already running")

// NewScheduler builds a scheduler. Nothing runs until Run or RunNow.
func NewScheduler(opts Options) *Scheduler {
	if opts.Bus == nil {
		opts.Bus = events.New("")
	}
	if opts.Tick == 0 {
		opts.Tick = time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Scheduler{opts: opts, states: map[string]*state{}, lastCheck: opts.Now()}
}

// Run ticks until ctx ends. Runs in progress are left to finish: a backup
// halfway through must not die because the supervisor restarted, and the
// runtime gives each one its own process group so it is not our child's
// problem either.
func (s *Scheduler) Run(ctx context.Context) {
	s.tick()
	t := time.NewTicker(s.opts.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick()
		}
	}
}

// Status is one task as the panel lists it.
type Status struct {
	Name     string    `json:"name"`
	Schedule string    `json:"schedule"`
	Site     string    `json:"site,omitempty"`
	Disabled bool      `json:"disabled,omitempty"`
	Next     time.Time `json:"next"`
	Last     *Run      `json:"last,omitempty"`
	Running  bool      `json:"running"`
	Error    string    `json:"error,omitempty"`
}

// List returns every task file, including the ones that do not parse, so
// a broken task is a line with an error and not an absence.
func (s *Scheduler) List() []Status {
	now := s.opts.Now()
	var out []Status
	for _, name := range Names(s.opts.Root) {
		st := Status{Name: name}
		t, err := Load(s.opts.Root, name)
		if err != nil {
			st.Error = err.Error()
		} else {
			st.Schedule, st.Site, st.Disabled = t.Schedule, t.Site, t.Disabled
			if sched, err := cron.Parse(t.Schedule); err == nil && !t.Disabled {
				st.Next = sched.Next(now)
			}
		}
		if r := LoadRecord(s.opts.Root, name); len(r.Runs) > 0 {
			last := r.Runs[len(r.Runs)-1]
			st.Last = &last
		}
		s.mu.Lock()
		if x := s.states[name]; x != nil {
			st.Running = x.running
		}
		s.mu.Unlock()
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RunNow runs a task outside its schedule and waits for it to end. It
// refuses while a scheduled run is in progress rather than queueing: the
// operator pressing the button wants to see it run, and "it will run
// later" is what the schedule already promises.
func (s *Scheduler) RunNow(name string) error {
	t, err := Load(s.opts.Root, name)
	if err != nil {
		return err
	}
	if !s.claim(name) {
		return ErrRunning
	}
	return s.execute(name, t, "operator")
}

func (s *Scheduler) tick() {
	now := s.opts.Now()
	s.mu.Lock()
	since := s.lastCheck
	s.lastCheck = now
	s.mu.Unlock()
	for _, name := range Names(s.opts.Root) {
		t, err := Load(s.opts.Root, name)
		if err != nil {
			s.invalid(name, err)
			continue
		}
		s.valid(name)
		if t.Disabled {
			continue
		}
		sched, err := cron.Parse(t.Schedule)
		if err != nil {
			s.invalid(name, err)
			continue
		}
		due := sched.Next(since)
		if due.IsZero() || due.After(now) {
			continue
		}
		s.trigger(name, t)
	}
}

func (s *Scheduler) invalid(name string, err error) {
	s.mu.Lock()
	st := s.state(name)
	changed := st.lastErr != err.Error()
	st.lastErr = err.Error()
	s.mu.Unlock()
	if changed {
		s.emit("task.invalid", name, "error", err.Error(), nil)
	}
}

func (s *Scheduler) valid(name string) {
	s.mu.Lock()
	st := s.state(name)
	was := st.lastErr
	st.lastErr = ""
	s.mu.Unlock()
	if was != "" {
		s.emit("task.valid", name, "", "task file readable again", nil)
	}
}

// state must be called with the lock held.
func (s *Scheduler) state(name string) *state {
	st, ok := s.states[name]
	if !ok {
		st = &state{}
		s.states[name] = st
	}
	return st
}

func (s *Scheduler) claim(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(name)
	if st.running {
		return false
	}
	st.running = true
	return true
}

// trigger starts a scheduled run, or applies the overlap rule when one is
// already running. A skip is recorded as a run with status "skipped": a
// task that silently never runs because the previous one is stuck is the
// failure that looks like success.
func (s *Scheduler) trigger(name string, t *Task) {
	if !s.claim(name) {
		if t.Overlap == "queue" {
			s.mu.Lock()
			already := s.state(name).queued
			s.state(name).queued = true
			s.mu.Unlock()
			if !already {
				s.emit("task.queued", name, "", "previous run still going; will run again when it ends", nil)
			}
			return
		}
		_ = appendRun(s.opts.Root, name, Run{At: s.opts.Now(), Status: StatusSkipped, Trigger: "schedule", Error: "previous run still going"})
		s.emit("task.skipped", name, "warn", "previous run still going", nil)
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = s.execute(name, t, "schedule")
	}()
}

// execute runs one claimed task to its end, records it, and releases the
// claim, running once more if a run was queued meanwhile.
func (s *Scheduler) execute(name string, t *Task, trigger string) error {
	err := s.runOnce(name, t, trigger)
	s.mu.Lock()
	st := s.state(name)
	again := st.queued
	st.queued = false
	st.running = false
	s.mu.Unlock()
	if again {
		if fresh, lerr := Load(s.opts.Root, name); lerr == nil && s.claim(name) {
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				_ = s.execute(name, fresh, "schedule")
			}()
		}
	}
	return err
}

// Wait blocks until every run the scheduler started has ended. Tests use
// it; the supervisor does not, on purpose (see Run).
func (s *Scheduler) Wait() { s.wg.Wait() }

func (s *Scheduler) spec(name string, t *Task) (*manifest.Spec, string, error) {
	root := s.opts.Root
	spec := &manifest.Spec{Domain: t.Site, SiteDir: root, Cwd: root, Env: map[string]string{}, Revision: "task:" + name}
	home := filepath.Join(root, "data", "homes", "tasks")
	if t.Site != "" {
		dir := filepath.Join(root, "sites", t.Site)
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return nil, "", fmt.Errorf("site %s: no such folder", t.Site)
		}
		m, err := manifest.Load(dir)
		if err != nil {
			return nil, "", fmt.Errorf("site %s: %w", t.Site, err)
		}
		spec.SiteDir, spec.Cwd = dir, dir
		home = filepath.Join(root, "data", "homes", t.Site)
		if m.Run != nil {
			resolved, err := manifest.Resolve(t.Site, dir, m)
			if err != nil {
				return nil, "", err
			}
			for k, v := range resolved.Env {
				spec.Env[k] = v
			}
		}
	}
	vars := map[string]string{"DOMAIN": t.Site, "SITE_DIR": spec.SiteDir}
	for k, v := range t.Env {
		spec.Env[k] = manifest.Expand(v, vars)
	}
	spec.Command = make([]string, len(t.Command))
	for i, a := range t.Command {
		spec.Command[i] = manifest.Expand(a, vars)
	}
	return spec, home, nil
}

func (s *Scheduler) runOnce(name string, t *Task, trigger string) error {
	start := s.opts.Now()
	record := func(status string, code int, err error) error {
		run := Run{At: start, Status: status, Duration: manifest.Duration(s.opts.Now().Sub(start)), ExitCode: code, Trigger: trigger}
		if err != nil {
			run.Error = err.Error()
		}
		if werr := appendRun(s.opts.Root, name, run); werr != nil {
			s.emit("task.failed", name, "error", "run not recorded: "+werr.Error(), nil)
		}
		data := map[string]any{"status": status, "exit_code": code, "duration": time.Duration(run.Duration).String(), "trigger": trigger}
		if status == StatusOK {
			s.emit("task.done", name, "", "", data)
			return nil
		}
		msg := status
		if err != nil {
			msg = err.Error()
		}
		s.emit("task.failed", name, "error", msg, data)
		if err == nil {
			err = fmt.Errorf("%s: %s (exit %d)", name, status, code)
		}
		return err
	}
	spec, home, err := s.spec(name, t)
	if err != nil {
		return record(StatusError, -1, err)
	}
	// Port 0: a task has no port, and the runtime puts PORT last anyway.
	// What the runtime gives in return is everything a site gets: its own
	// process group, output to a file nobody pipes, SIGTERM then SIGKILL.
	inst, err := runtime.Process{}.Start(context.Background(), spec, 0, LogPath(s.opts.Root, name), home)
	if err != nil {
		return record(StatusError, -1, err)
	}
	s.emit("task.started", name, "", "", map[string]any{"trigger": trigger, "pid": inst.State().PID, "site": t.Site})
	timeout := time.Duration(t.Timeout)
	var timer <-chan time.Time
	if timeout > 0 {
		timer = time.After(timeout)
	}
	select {
	case err := <-inst.Done():
		code := exitCode(err)
		if err == nil {
			return record(StatusOK, 0, nil)
		}
		return record(StatusFailed, code, nil)
	case <-timer:
		_ = inst.Stop(context.Background(), 10*time.Second)
		return record(StatusTimeout, -1, fmt.Errorf("killed after %s", timeout))
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func (s *Scheduler) emit(typ, name, level, msg string, data map[string]any) {
	s.opts.Bus.Emit(events.Event{Type: typ, Subject: name, Actor: "schedule", Level: level, Message: msg, Data: data})
}
