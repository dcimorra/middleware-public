// Package vitals watches how the machine is doing: processor, memory,
// swap and disk, every few seconds, from /proc and statfs. A panel that
// only says whether the sites answer does not say why they suddenly
// answer slowly, and the answer is usually here: a build eating every
// core, a process left spinning, the disk filling up.
//
// A stretch under strain is an episode, and what it keeps is who: "at
// 04:12 the CPU was at 97%" is no use the next day, when the process no
// longer exists. So while it lasts, the processes spending most are taken
// down every few samples, with the site each belongs to.
package vitals

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
)

// Sample is one look at the machine.
type Sample struct {
	At time.Time `json:"at"`
	// CPU is the share busy since the previous sample, 0 to 100 over every
	// core: who is working, not who is waiting (that is the load).
	CPU   float64 `json:"cpu"`
	Cores int     `json:"cores"`
	// IOWait is apart: 90% busy of which 60 waiting on the disk is not a
	// processor problem, and mixing the two sends one to the wrong place.
	IOWait   float64 `json:"iowait"`
	Load1    float64 `json:"load1"`
	MemUsed  uint64  `json:"mem_used"`
	MemTotal uint64  `json:"mem_total"`
	MemPct   float64 `json:"mem_pct"`
	// Swap: a machine that starts swapping is about to be slow, and it shows
	// in no other number.
	SwapUsed  uint64  `json:"swap_used"`
	SwapTotal uint64  `json:"swap_total"`
	DiskUsed  uint64  `json:"disk_used"`
	DiskTotal uint64  `json:"disk_total"`
	DiskPct   float64 `json:"disk_pct"`
}

// Proc is one process in an episode's shot.
type Proc struct {
	PID  int     `json:"pid"`
	CPU  float64 `json:"cpu"` // percent of one core
	RSS  uint64  `json:"rss"`
	Cmd  string  `json:"cmd"`
	Site string  `json:"site,omitempty"`
}

// Shot is the processes spending most at one moment of an episode.
type Shot struct {
	At    time.Time `json:"at"`
	CPU   float64   `json:"cpu"`
	Procs []Proc    `json:"procs"`
}

// Blame is a process seen at the top of several shots: the summary read
// first.
type Blame struct {
	Cmd   string  `json:"cmd"`
	Site  string  `json:"site,omitempty"`
	Times int     `json:"times"`
	CPU   float64 `json:"cpu"` // average percent of one core when seen
}

// Episode is a stretch under strain.
type Episode struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to,omitzero"`
	Reason  string    `json:"reason"` // "cpu", "memory", "swap"
	PeakCPU float64   `json:"peak_cpu"`
	AvgCPU  float64   `json:"avg_cpu"`
	PeakMem float64   `json:"peak_mem"`
	IOWait  float64   `json:"iowait_max"`
	Blamed  []Blame   `json:"blamed,omitempty"`
	Shots   []Shot    `json:"shots,omitempty"`
	sum     float64
	n       int
}

// Thresholds of an episode.
const (
	CPULine    = 85.0 // percent of every core
	MemoryLine = 90.0 // percent of memory
	every      = 5 * time.Second
	keep       = 720 // an hour of samples
	openAfter  = 6   // 30 seconds above the line
	closeAfter = 12  // a minute below it
	shotEvery  = 3   // a shot every 15 seconds while open
	maxShots   = 40
	keepEpis   = 50
	topProcs   = 8
)

// Options configure a Monitor.
type Options struct {
	DataDir  string
	SitesDir string
	Bus      *events.Bus
	// Proc is /proc; tests point it elsewhere.
	Proc string
}

// Monitor samples and keeps the last hour and the episodes.
type Monitor struct {
	opts     Options
	mu       sync.Mutex
	history  []Sample
	episodes []Episode
	open     *Episode
	above    int
	below    int
	prevCPU  cpuTimes
	prevProc map[int]uint64
	prevAt   time.Time
}

// New builds a monitor, reading the episodes a previous run kept.
func New(opts Options) *Monitor {
	if opts.Proc == "" {
		opts.Proc = "/proc"
	}
	m := &Monitor{opts: opts, prevProc: map[int]uint64{}}
	_ = fsx.ReadJSON(m.path(), &m.episodes)
	return m
}

func (m *Monitor) path() string { return filepath.Join(m.opts.DataDir, "vitals", "episodes.json") }

// Run samples until ctx ends.
func (m *Monitor) Run(done <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	m.sample(time.Now())
	for {
		select {
		case <-done:
			return
		case now := <-t.C:
			m.sample(now)
		}
	}
}

// Now is the last sample, History the last hour, Episodes the last ones,
// newest first, the open one included.
func (m *Monitor) Now() Sample {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.history) == 0 {
		return Sample{}
	}
	return m.history[len(m.history)-1]
}

func (m *Monitor) History() []Sample {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Sample(nil), m.history...)
}

func (m *Monitor) Episodes() []Episode {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Episode{}
	if m.open != nil {
		out = append(out, *m.open)
	}
	for i := len(m.episodes) - 1; i >= 0; i-- {
		out = append(out, m.episodes[i])
	}
	return out
}

type cpuTimes struct{ total, idle, iowait uint64 }

func (m *Monitor) readCPU() (cpuTimes, int) {
	f, err := os.Open(filepath.Join(m.opts.Proc, "stat"))
	if err != nil {
		return cpuTimes{}, 0
	}
	defer f.Close()
	var t cpuTimes
	cores := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 {
			continue
		}
		if fields[0] == "cpu" {
			for i, v := range fields[1:] {
				n, _ := strconv.ParseUint(v, 10, 64)
				// guest and guest_nice are already inside user and nice.
				if i < 8 {
					t.total += n
				}
				if i == 3 {
					t.idle += n
				}
				if i == 4 {
					t.idle += n
					t.iowait = n
				}
			}
		} else if strings.HasPrefix(fields[0], "cpu") {
			cores++
		}
	}
	return t, cores
}

func (m *Monitor) readMem(s *Sample) {
	f, err := os.Open(filepath.Join(m.opts.Proc, "meminfo"))
	if err != nil {
		return
	}
	defer f.Close()
	vals := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		vals[k] = n * 1024
	}
	s.MemTotal = vals["MemTotal"]
	if avail, ok := vals["MemAvailable"]; ok && s.MemTotal >= avail {
		s.MemUsed = s.MemTotal - avail
	}
	if s.MemTotal > 0 {
		s.MemPct = 100 * float64(s.MemUsed) / float64(s.MemTotal)
	}
	s.SwapTotal = vals["SwapTotal"]
	if s.SwapTotal >= vals["SwapFree"] {
		s.SwapUsed = s.SwapTotal - vals["SwapFree"]
	}
}

func (m *Monitor) sample(now time.Time) {
	s := Sample{At: now.UTC()}
	cur, cores := m.readCPU()
	s.Cores = cores
	if d := cur.total - m.prevCPU.total; m.prevCPU.total > 0 && d > 0 {
		s.CPU = 100 * float64(d-(cur.idle-m.prevCPU.idle)) / float64(d)
		s.IOWait = 100 * float64(cur.iowait-m.prevCPU.iowait) / float64(d)
	}
	m.prevCPU = cur
	if b, err := os.ReadFile(filepath.Join(m.opts.Proc, "loadavg")); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			s.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	m.readMem(&s)
	var st syscall.Statfs_t
	if syscall.Statfs(m.opts.DataDir, &st) == nil {
		s.DiskTotal = st.Blocks * uint64(st.Bsize)
		s.DiskUsed = s.DiskTotal - st.Bavail*uint64(st.Bsize)
		if s.DiskTotal > 0 {
			s.DiskPct = 100 * float64(s.DiskUsed) / float64(s.DiskTotal)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.history = append(m.history, s)
	if len(m.history) > keep {
		m.history = m.history[len(m.history)-keep:]
	}
	m.judge(s)
}

// judge opens, feeds and closes episodes. Called under m.mu.
func (m *Monitor) judge(s Sample) {
	reason := ""
	switch {
	case s.CPU >= CPULine:
		reason = "cpu"
	case s.MemPct >= MemoryLine:
		reason = "memory"
	case s.SwapTotal > 0 && float64(s.SwapUsed) > 0.5*float64(s.SwapTotal):
		reason = "swap"
	}
	if reason != "" {
		m.above++
		m.below = 0
	} else {
		m.below++
		m.above = 0
	}
	if m.open == nil {
		if m.above >= openAfter {
			m.open = &Episode{From: s.At.Add(-time.Duration(openAfter-1) * every), Reason: reason}
			m.emit("vitals.episode", "warn", "the machine is under strain ("+reason+")")
		}
		if m.open == nil {
			m.procs(false) // keeps the counters warm for the first shot
			return
		}
	}
	e := m.open
	e.sum += s.CPU
	e.n++
	e.AvgCPU = e.sum / float64(e.n)
	e.PeakCPU = max(e.PeakCPU, s.CPU)
	e.PeakMem = max(e.PeakMem, s.MemPct)
	e.IOWait = max(e.IOWait, s.IOWait)
	if e.n%shotEvery == 1 && len(e.Shots) < maxShots {
		e.Shots = append(e.Shots, Shot{At: s.At, CPU: s.CPU, Procs: m.procs(true)})
		e.Blamed = blame(e.Shots)
	} else {
		m.procs(false)
	}
	if m.below >= closeAfter {
		e.To = s.At
		m.episodes = append(m.episodes, *e)
		if len(m.episodes) > keepEpis {
			m.episodes = m.episodes[len(m.episodes)-keepEpis:]
		}
		m.open = nil
		_ = os.MkdirAll(filepath.Dir(m.path()), 0o755)
		_ = fsx.WriteJSON(m.path(), m.episodes, 0o644)
		m.emit("vitals.episode_end", "", "the machine is back to normal")
	}
}

func (m *Monitor) emit(typ, level, msg string) {
	if m.opts.Bus != nil {
		m.opts.Bus.Emit(events.Event{Type: typ, Actor: "engine", Level: level, Message: msg})
	}
}

// procs reads every process's CPU time; with take, it returns the ones
// that spent most since the last read, with their site.
func (m *Monitor) procs(take bool) []Proc {
	ents, err := os.ReadDir(m.opts.Proc)
	if err != nil {
		return nil
	}
	now := time.Now()
	secs := now.Sub(m.prevAt).Seconds()
	m.prevAt = now
	ticks := map[int]uint64{}
	var out []Proc
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(m.opts.Proc, e.Name(), "stat"))
		if err != nil {
			continue
		}
		// The command is between parentheses and may hold spaces.
		i := strings.LastIndexByte(string(b), ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(string(b[i+1:]))
		if len(f) < 22 {
			continue
		}
		ut, _ := strconv.ParseUint(f[11], 10, 64)
		st, _ := strconv.ParseUint(f[12], 10, 64)
		rss, _ := strconv.ParseUint(f[21], 10, 64)
		ticks[pid] = ut + st
		if !take || secs <= 0 {
			continue
		}
		prev, ok := m.prevProc[pid]
		if !ok || ut+st <= prev {
			continue
		}
		cpu := 100 * float64(ut+st-prev) / 100 / secs
		out = append(out, Proc{PID: pid, CPU: cpu, RSS: rss * uint64(os.Getpagesize())})
	}
	m.prevProc = ticks
	if !take {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CPU > out[j].CPU })
	if len(out) > topProcs {
		out = out[:topProcs]
	}
	for i := range out {
		out[i].Cmd = cmdline(filepath.Join(m.opts.Proc, strconv.Itoa(out[i].PID)))
		out[i].Site = m.siteOf(out[i].PID)
	}
	return out
}

func cmdline(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil || len(b) == 0 {
		b, _ = os.ReadFile(filepath.Join(dir, "comm"))
	}
	s := strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// siteOf is the site whose folder the process works in, or "".
func (m *Monitor) siteOf(pid int) string {
	if m.opts.SitesDir == "" {
		return ""
	}
	cwd, err := os.Readlink(filepath.Join(m.opts.Proc, strconv.Itoa(pid), "cwd"))
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(m.opts.SitesDir, cwd)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	site, _, _ := strings.Cut(rel, string(filepath.Separator))
	return site
}

// blame sums the shots: who was at the top, how often, how hard.
func blame(shots []Shot) []Blame {
	by := map[string]*Blame{}
	for _, s := range shots {
		for _, p := range s.Procs {
			if p.CPU < 5 {
				continue
			}
			k := p.Site + "\x00" + p.Cmd
			b := by[k]
			if b == nil {
				b = &Blame{Cmd: p.Cmd, Site: p.Site}
				by[k] = b
			}
			b.CPU = (b.CPU*float64(b.Times) + p.CPU) / float64(b.Times+1)
			b.Times++
		}
	}
	out := make([]Blame, 0, len(by))
	for _, b := range by {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Times != out[j].Times {
			return out[i].Times > out[j].Times
		}
		return out[i].CPU > out[j].CPU
	})
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}
