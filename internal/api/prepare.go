package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dcimorra/middleware-public/internal/relocate"
)

// A received site's install runs on its own here, not inside the request
// that asked for it. The first version did, under the request's context:
// a move from another node failed with "npm ci: signal: killed" and no
// output after about three minutes, which is what exec does to a command
// whose context ends, and a request between two nodes can end for reasons
// neither sees (a NAT that drops a quiet connection). Now the request
// starts it and the moving node asks how it goes; the install has its own
// time and nothing on the network can cut it.

// prepareJob is one site's install in its staging.
type prepareJob struct {
	Done   bool   `json:"done"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

type prepareJobs struct {
	mu   sync.Mutex
	jobs map[string]*prepareJob
}

// prepareTime bounds an install: a big site on a slow machine takes
// minutes; past this something is wrong.
const prepareTime = 30 * time.Minute

func (s *Server) registerPrepare(rc relocate.Receiver) {
	s.Handle("POST", Prefix+"/relocate/{domain}/prepare", "Start installing a received site's dependencies in its staging, for this machine, before it is stopped where it is; GET says how it goes", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain := r.PathValue("domain")
		dir := rc.StagingSite(domain)
		if _, err := os.Stat(dir); err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", relocate.ErrNoStaging)
		}
		// Where the source left node_modules behind, each relative to the
		// site; none said (an older node) is its root, as before.
		var body struct {
			Dirs []string `json:"dirs"`
		}
		if r.ContentLength != 0 {
			if err := ReadJSON(r, &body); err != nil {
				return nil, err
			}
		}
		if len(body.Dirs) == 0 {
			body.Dirs = []string{"."}
		}
		var targets []string
		for _, d := range body.Dirs {
			c := filepath.Clean(filepath.FromSlash(d))
			if filepath.IsAbs(c) || c == ".." || strings.HasPrefix(c, ".."+string(filepath.Separator)) {
				return nil, Errorf(http.StatusBadRequest, "dirs: %q is not inside the site", d)
			}
			targets = append(targets, c)
		}
		s.prepares.mu.Lock()
		defer s.prepares.mu.Unlock()
		if s.prepares.jobs == nil {
			s.prepares.jobs = map[string]*prepareJob{}
		}
		if j := s.prepares.jobs[domain]; j != nil && !j.Done {
			return map[string]any{"started": false, "running": true}, nil
		}
		job := &prepareJob{}
		s.prepares.jobs[domain] = job
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), prepareTime)
			defer cancel()
			var saids []string
			var err error
			for _, c := range targets {
				var said string
				said, err = installDeps(ctx, filepath.Join(dir, c))
				if c != "." {
					said = filepath.ToSlash(c) + ": " + said
					if err != nil {
						err = fmt.Errorf("%s: %w", filepath.ToSlash(c), err)
					}
				}
				if err != nil {
					break
				}
				saids = append(saids, said)
			}
			s.prepares.mu.Lock()
			job.Done, job.Output = true, strings.Join(saids, " · ")
			if err != nil {
				job.Error = err.Error()
			}
			s.prepares.mu.Unlock()
		}()
		return map[string]any{"started": true}, nil
	})
	s.Handle("GET", Prefix+"/relocate/{domain}/prepare", "How the install of a received site's dependencies goes: done, what it said, or why it failed", func(w http.ResponseWriter, r *http.Request) (any, error) {
		s.prepares.mu.Lock()
		defer s.prepares.mu.Unlock()
		j := s.prepares.jobs[r.PathValue("domain")]
		if j == nil {
			return nil, Errorf(http.StatusNotFound, "no install of %s here since this node started", r.PathValue("domain"))
		}
		return *j, nil
	})
}

// oomKills is the kernel's count of processes it killed for memory, from
// /proc/vmstat (any user may read it); -1 when it cannot be read.
func oomKills() int {
	raw, err := os.ReadFile("/proc/vmstat")
	if err != nil {
		return -1
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(l, "oom_kill "); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err == nil {
				return n
			}
		}
	}
	return -1
}

// memory is the machine's total and available memory, in MB.
func memory() (total, avail int) {
	raw, _ := os.ReadFile("/proc/meminfo")
	for _, l := range strings.Split(string(raw), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		kb, _ := strconv.Atoi(f[1])
		switch f[0] {
		case "MemTotal:":
			total = kb / 1024
		case "MemAvailable:":
			avail = kb / 1024
		}
	}
	return total, avail
}

// whyKilled says what ended a command that died: its context, the kernel
// for memory, or a signal from someone else. A bare "signal: killed" with
// no output was all the first failure said.
func whyKilled(ctx context.Context, err error, oomBefore int) string {
	if ctx.Err() != nil {
		return fmt.Sprintf("it was cut after %s (%v)", prepareTime, ctx.Err())
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return ""
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return ""
	}
	if ws.Signal() == syscall.SIGKILL {
		if after := oomKills(); oomBefore >= 0 && after > oomBefore {
			total, avail := memory()
			return fmt.Sprintf("the kernel killed it for lack of memory (oom_kill went from %d to %d); this machine has %d MB, %d MB available now: give it more memory or swap, or install with the site in a container", oomBefore, after, total, avail)
		}
		return "something on this machine sent it SIGKILL (not the kernel's out-of-memory killer, whose count did not move)"
	}
	return "it ended with " + ws.Signal().String()
}
