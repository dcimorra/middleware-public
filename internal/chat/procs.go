package chat

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// MarkVar is put in every turn's environment, and everything a turn
// starts inherits it, also what escapes the turn's process group with
// setsid on purpose (a build, a dev server, as the skill tells agents to
// do for long work). The group kill at the end of a turn cannot reach
// those; the mark is how the house still finds them. V1 learned it from
// luma32.com, whose agents left headless browsers and test servers running
// for days, one of them on more than a core: the site could not clean them
// up because it runs in a container and does not see them.
const MarkVar = "MIDDLEWARE_CHAT_ID"

// TurnLimit cuts a turn that runs this long; zero, the default, never
// cuts one. A two-hour cut lost the long jobs operators leave running
// overnight; a turn stuck in a loop is stopped from the panel, and what it
// left running is listed under its processes either way.
var TurnLimit time.Duration

// Proc is a process a conversation left running.
type Proc struct {
	PID     int       `json:"pid"`
	Command string    `json:"command"`
	Started time.Time `json:"started,omitempty"`
}

// Processes lists what the conversation of domain has running now: the
// turn itself and whatever it left behind.
func (m *Manager) Processes(domain string) []Proc {
	return marked(m.procRoot(), []byte(MarkVar+"="+domain+"\x00"))
}

// Reap ends the turn of domain if one runs and kills everything its
// conversation left alive. It returns how many processes were signalled.
func (m *Manager) Reap(domain string) int {
	_ = m.Stop(domain)
	return reap(m.procRoot(), []byte(MarkVar+"="+domain+"\x00"), 0)
}

// Kill ends one process the conversation of domain left, and refuses any
// other: the permission is checked by the house, never by the caller.
func (m *Manager) Kill(domain string, pid int) bool {
	return reap(m.procRoot(), []byte(MarkVar+"="+domain+"\x00"), pid) > 0
}

func (m *Manager) procRoot() string {
	if m.opts.ProcRoot != "" {
		return m.opts.ProcRoot
	}
	return "/proc"
}

// marked are this uid's processes whose environment carries mark.
func marked(procRoot string, mark []byte) []Proc {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var out []Proc
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		env, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "environ"))
		if err != nil || !bytes.Contains(append(env, 0), mark) {
			continue
		}
		cmd, _ := os.ReadFile(filepath.Join(procRoot, e.Name(), "cmdline"))
		p := Proc{PID: pid, Command: strings.TrimSpace(strings.ReplaceAll(string(cmd), "\x00", " "))}
		if st, err := os.Stat(filepath.Join(procRoot, e.Name())); err == nil {
			p.Started = st.ModTime()
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// reap sends SIGTERM to every marked process (only to `only` when it is
// not 0), and SIGKILL to what is left after a grace. A group leader gets
// it on the whole group, which is where what it started hangs.
func reap(procRoot string, mark []byte, only int) int {
	var pids []int
	for _, p := range marked(procRoot, mark) {
		if only == 0 || p.PID == only {
			pids = append(pids, p.PID)
		}
	}
	signal := func(sig syscall.Signal, pid int) {
		if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
			_ = syscall.Kill(-pid, sig)
		}
		_ = syscall.Kill(pid, sig)
	}
	for _, pid := range pids {
		signal(syscall.SIGTERM, pid)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && anyAlive(pids) {
		time.Sleep(100 * time.Millisecond)
	}
	for _, pid := range pids {
		if syscall.Kill(pid, 0) == nil {
			signal(syscall.SIGKILL, pid)
		}
	}
	return len(pids)
}

func anyAlive(pids []int) bool {
	for _, pid := range pids {
		if syscall.Kill(pid, 0) == nil && !zombie(pid) {
			return true
		}
	}
	return false
}

func zombie(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	i := bytes.LastIndexByte(data, ')')
	return i < 0 || i+2 >= len(data) || data[i+2] == 'Z'
}
