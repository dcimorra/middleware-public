package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// CLIRun is the one subprocess runner both adapters share: a child in its
// own process group, stdout read line by line through a parser, stderr
// kept in a bounded buffer for the error event, and an end that never
// waits on a grandchild. The stdout pipe is read by us and only by us, and
// the moment the parser says the run is over the child gets a short grace
// and then SIGKILL to its whole group: a tool it left running (a dev
// server started from a Bash call) must not keep the turn open forever.
type CLIRun struct {
	events chan Event
	cmd    *exec.Cmd
	stop   chan struct{}
	once   sync.Once
	stderr strings.Builder
	errMu  sync.Mutex
}

// Parser turns one stdout line into zero or more events and says whether
// the line was the run's final one.
type Parser interface {
	Line(line string) (evs []Event, done bool)
	// Flush emits what a stream that ended without a final line still
	// holds (a json output format produces a single document at exit).
	Flush() []Event
}

// StartCLI launches bin with args in dir and streams its output through p.
// The caller's Jail is already translated into args by the adapter.
func StartCLI(ctx context.Context, p Parser, dir string, env []string, bin string, args ...string) (*CLIRun, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", bin, err)
	}
	cmd := exec.Command(path, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	r := &CLIRun{events: make(chan Event, 64), cmd: cmd, stop: make(chan struct{})}
	go r.readStderr(stderr)
	go r.loop(ctx, p, stdout)
	return r, nil
}

// Events implements Run.
func (r *CLIRun) Events() <-chan Event { return r.events }

// Stop implements Run.
func (r *CLIRun) Stop() {
	r.once.Do(func() { close(r.stop) })
}

func (r *CLIRun) readStderr(f io.Reader) {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		r.errMu.Lock()
		if r.stderr.Len() < 16*1024 {
			r.stderr.WriteString(sc.Text())
			r.stderr.WriteByte('\n')
		}
		r.errMu.Unlock()
	}
}

func (r *CLIRun) stderrText() string {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	return strings.TrimSpace(r.stderr.String())
}

func (r *CLIRun) loop(ctx context.Context, p Parser, stdout io.Reader) {
	defer close(r.events)
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(stdout)
		// A single assistant message carrying a large file in a tool input
		// can run to megabytes; a line the scanner refuses would end the
		// run with a misleading error.
		sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	sawEnd := false
	stopped := false
	emit := func(evs []Event) {
		for _, ev := range evs {
			if ev.Kind == KindResult || ev.Kind == KindError {
				sawEnd = true
			}
			r.send(ev)
		}
	}
read:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				break read
			}
			evs, done := p.Line(line)
			emit(evs)
			if done {
				break read
			}
		case <-r.stop:
			stopped = true
			break read
		case <-ctx.Done():
			stopped = true
			break read
		}
	}
	// Whatever the child still writes after this point is drained and
	// dropped, so the scanner goroutine can reach the pipe's end once the
	// process is gone instead of blocking on a channel nobody reads.
	go func() {
		for range lines {
		}
	}()
	if !stopped {
		emit(p.Flush())
	}
	exitErr := r.wait(stopped)
	if stopped {
		r.send(Event{Kind: KindError, Text: "stopped"})
		return
	}
	if !sawEnd {
		text := "agent exited without a result"
		if exitErr != nil {
			text = exitErr.Error()
		}
		if se := r.stderrText(); se != "" {
			text += ": " + se
		}
		r.send(Event{Kind: KindError, Text: text})
	}
}

// wait reaps the child. After a clean end it gets three seconds to exit on
// its own; a stop kills at once. Either way the whole group goes, and Wait
// is bounded because the pipe is ours, not a writer handed to exec.Cmd.
func (r *CLIRun) wait(stopped bool) error {
	done := make(chan error, 1)
	go func() { done <- r.cmd.Wait() }()
	grace := 3 * time.Second
	if stopped {
		_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGTERM)
		grace = 2 * time.Second
	}
	select {
	case err := <-done:
		return err
	case <-time.After(grace):
	}
	_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		return errors.New("agent process survived SIGKILL")
	}
}

func (r *CLIRun) send(ev Event) {
	select {
	case r.events <- ev:
	case <-time.After(30 * time.Second):
		// A consumer that stopped reading must not pin the process: the
		// event is dropped and the run goes on to its end.
	}
}
