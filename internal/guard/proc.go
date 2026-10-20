package guard

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Process is one line of the process table as the guard sees it.
type Process struct {
	PID     int
	PPID    int
	PGID    int
	UID     int
	Comm    string
	State   byte
	Cmdline []string
	Cwd     string
	Exe     string
	Started time.Time
	// CPUTicks is user plus system time so far, in clock ticks; RSS the
	// resident memory in bytes. Summed per site for its resource history.
	CPUTicks int64
	RSS      int64
	// Listening holds the TCP ports this process has open in LISTEN state.
	// It is filled only for processes the guard attributes to a site,
	// because mapping sockets to pids means reading every fd they own.
	Listening []int
}

// Reader reads a proc filesystem. Root is /proc on a real machine and a
// folder built by hand in tests: the layout (<pid>/stat, cmdline, cwd, exe,
// fd/*, net/tcp, stat) is the kernel's, so what the tests exercise is what
// runs in production.
type Reader struct {
	Root string
	// ClockTicks is the kernel's USER_HZ; 100 on every Linux build that
	// matters, and not readable without cgo.
	ClockTicks int
}

func (r Reader) root() string {
	if r.Root == "" {
		return "/proc"
	}
	return r.Root
}

func (r Reader) ticks() int {
	if r.ClockTicks <= 0 {
		return 100
	}
	return r.ClockTicks
}

// Processes reads every process. Processes that vanish while being read
// are skipped, not reported: the table changes under the reader's feet.
func (r Reader) Processes() ([]Process, error) {
	entries, err := os.ReadDir(r.root())
	if err != nil {
		return nil, err
	}
	boot := r.bootTime()
	var out []Process
	for _, ent := range entries {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil || pid <= 0 || !ent.IsDir() {
			continue
		}
		p, ok := r.read(pid, boot)
		if ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func (r Reader) read(pid int, boot time.Time) (Process, bool) {
	dir := filepath.Join(r.root(), strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return Process{}, false
	}
	p := Process{PID: pid}
	// The command name is in parentheses and may itself hold spaces and
	// parentheses, so the split starts after the last ')'.
	open := bytes.IndexByte(stat, '(')
	closeIdx := bytes.LastIndexByte(stat, ')')
	if open < 0 || closeIdx < open {
		return Process{}, false
	}
	p.Comm = string(stat[open+1 : closeIdx])
	fields := strings.Fields(string(stat[closeIdx+1:]))
	if len(fields) < 20 {
		return Process{}, false
	}
	p.State = fields[0][0]
	p.PPID, _ = strconv.Atoi(fields[1])
	p.PGID, _ = strconv.Atoi(fields[2])
	ut, _ := strconv.ParseInt(fields[11], 10, 64)
	st, _ := strconv.ParseInt(fields[12], 10, 64)
	p.CPUTicks = ut + st
	if len(fields) > 21 {
		if pages, err := strconv.ParseInt(fields[21], 10, 64); err == nil {
			p.RSS = pages * int64(os.Getpagesize())
		}
	}
	if start, err := strconv.ParseInt(fields[19], 10, 64); err == nil && !boot.IsZero() {
		p.Started = boot.Add(time.Duration(start) * time.Second / time.Duration(r.ticks()))
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
		for _, a := range bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0}) {
			p.Cmdline = append(p.Cmdline, string(a))
		}
	}
	p.Cwd, _ = os.Readlink(filepath.Join(dir, "cwd"))
	p.Exe, _ = os.Readlink(filepath.Join(dir, "exe"))
	p.Exe = strings.TrimSuffix(p.Exe, " (deleted)")
	p.UID = -1
	if status, err := os.ReadFile(filepath.Join(dir, "status")); err == nil {
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "Uid:") {
				if f := strings.Fields(line); len(f) > 1 {
					p.UID, _ = strconv.Atoi(f[1])
				}
				break
			}
		}
	}
	return p, true
}

// bootTime reads btime from <root>/stat; zero when unavailable.
func (r Reader) bootTime() time.Time {
	f, err := os.Open(filepath.Join(r.root(), "stat"))
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "btime ") {
			n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(sc.Text(), "btime ")), 10, 64)
			if err == nil {
				return time.Unix(n, 0)
			}
		}
	}
	return time.Time{}
}

// listeners maps socket inode → local port for every TCP socket in LISTEN
// state, from net/tcp and net/tcp6.
func (r Reader) listeners() map[uint64]int {
	out := map[uint64]int{}
	for _, name := range []string{"net/tcp", "net/tcp6"} {
		f, err := os.Open(filepath.Join(r.root(), name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Scan() // header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			// sl local_address rem_address st tx_queue:rx_queue tr:tm->when retrnsmt uid timeout inode
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			_, portHex, ok := strings.Cut(fields[1], ":")
			if !ok {
				continue
			}
			port, err := strconv.ParseInt(portHex, 16, 32)
			if err != nil {
				continue
			}
			inode, err := strconv.ParseUint(fields[9], 10, 64)
			if err != nil {
				continue
			}
			out[inode] = int(port)
		}
		f.Close()
	}
	return out
}

// fillListening resolves the listening ports of the given processes by
// reading their fd links. It is called for site processes only.
func (r Reader) fillListening(procs []*Process) {
	if len(procs) == 0 {
		return
	}
	byInode := r.listeners()
	if len(byInode) == 0 {
		return
	}
	for _, p := range procs {
		fdDir := filepath.Join(r.root(), strconv.Itoa(p.PID), "fd")
		entries, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		seen := map[int]bool{}
		for _, ent := range entries {
			link, err := os.Readlink(filepath.Join(fdDir, ent.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			inode, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]"), 10, 64)
			if err != nil {
				continue
			}
			if port, ok := byInode[inode]; ok && !seen[port] {
				seen[port] = true
				p.Listening = append(p.Listening, port)
			}
		}
	}
}

// connected is the set of local TCP ports with an established connection,
// from net/tcp and net/tcp6: a browser's debugging port in it has a client
// on the other end.
func (r Reader) connected() map[int]bool {
	out := map[int]bool{}
	for _, name := range []string{"net/tcp", "net/tcp6"} {
		f, err := os.Open(filepath.Join(r.root(), name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Scan()
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 4 || fields[3] != "01" {
				continue
			}
			if _, portHex, ok := strings.Cut(fields[1], ":"); ok {
				if port, err := strconv.ParseInt(portHex, 16, 32); err == nil {
					out[int(port)] = true
				}
			}
		}
		f.Close()
	}
	return out
}
