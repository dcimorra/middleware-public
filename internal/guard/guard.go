// Package guard is the watchdog: every three seconds it reads the process
// table, attributes processes to the sites that spawned them, and checks
// each against rules written in code. A suspicious finding is an event; an
// intrusion kills the process tree and switches the site off through its
// manifest, so the decision survives a restart and the operator finds the
// site stopped with an incident that says why.
//
// The guard decides with rules, never with a model: it is the one place in
// the house that presses a button on its own, and a button pressed by
// something that can be talked into it is not a safeguard.
package guard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/runtime"
)

// SiteInfo is what the guard needs to know about a site.
type SiteInfo struct {
	Domain string
	Dir    string
	// Ports the engine assigned: the instance's and the workers'.
	Ports []int
	// Home is the HOME the engine gives the site's processes
	// (data/homes/<domain>); what is inside it is the site's own.
	Home string
	// Logs are the files the site's processes write (the instance's and
	// the workers'), scanned for new lines on every sweep.
	Logs []string
}

// Incident is one finding, kept at data/guard/incidents/<id>.json.
type Incident struct {
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	Domain    string    `json:"domain"`
	Rule      string    `json:"rule"`
	Level     string    `json:"level"`
	Where     string    `json:"where"`
	Action    string    `json:"action"`
	PID       int       `json:"pid,omitempty"`
	Cmdline   string    `json:"cmdline,omitempty"`
	Dismissed bool      `json:"dismissed,omitempty"`
}

// Options configure a Guard.
type Options struct {
	// ProcRoot is /proc, or a fake tree in tests.
	ProcRoot string
	// DataDir is data/: state/<domain>.json is read, guard/incidents/ written.
	DataDir  string
	SitesDir string
	// Sites returns the sites and their assigned ports.
	Sites func() []SiteInfo
	// Agents are the pids of the agent turns running now (chat.Manager
	// knows them). They and what descends from them are measured with
	// their site but not judged by the rules: an agent works in its
	// site's folder with a prompt that names others', and the jail, not
	// the guard, is what bounds it.
	Agents func() []int
	// PortRange is the engine's allocation range.
	PortRange [2]int
	Bus       *events.Bus
	// Kill ends one process; nil sends SIGKILL.
	Kill     func(pid int) error
	Interval time.Duration
	Now      func() time.Time
	// OrphanAge is how long a parentless browser may live; thirty minutes.
	OrphanAge time.Duration
	// Contained is called after an intrusion has been acted on (the
	// processes killed, the site disabled), to ask an agent to find out
	// how it got in. Called in its own goroutine; nil does nothing.
	Contained func(Incident)
}

// Guard is the watchdog.
type Guard struct {
	opts   Options
	reader Reader
	now    func() time.Time
	kill   func(pid int) error

	mu sync.Mutex
	// seen remembers (domain, rule, pid) findings so a suspicious process
	// that stays alive is reported once an hour, not every thirty seconds.
	seen map[string]time.Time
	// offsets is how far each site log has been read.
	offsets map[string]int64
	// res is each site's resource meter (resources.go).
	res meters
}

// New builds a guard. Nothing runs until Run or Sweep.
func New(opts Options) *Guard {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Interval <= 0 {
		// Three seconds, not thirty: V1's credential theft lived for
		// milliseconds and a thirty-second sweep saw its aftermath at best.
		// Reading /proc is cheap; sockets are read only for site processes.
		opts.Interval = 3 * time.Second
	}
	if opts.OrphanAge <= 0 {
		opts.OrphanAge = 30 * time.Minute
	}
	g := &Guard{opts: opts, reader: Reader{Root: opts.ProcRoot}, now: opts.Now, seen: map[string]time.Time{}, offsets: map[string]int64{}}
	g.kill = opts.Kill
	if g.kill == nil {
		g.kill = func(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
	}
	return g
}

// Run sweeps every interval until ctx ends. It returns at once.
func (g *Guard) Run(ctx context.Context) {
	go func() {
		t := time.NewTicker(g.opts.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := g.Sweep(); err != nil {
					g.emit(events.Event{Type: "guard.error", Actor: "guard", Level: "warn", Message: err.Error()})
				}
			}
		}
	}()
}

// Rules lists the rules.
func (g *Guard) Rules() []Rule { return Rules() }

func (g *Guard) incidentsDir() string {
	return filepath.Join(g.opts.DataDir, "guard", "incidents")
}

// Incidents returns every recorded incident, newest first.
func (g *Guard) Incidents() ([]Incident, error) { return ReadIncidents(g.opts.DataDir) }

// ReadIncidents reads the incidents under dataDir, newest first. The CLI
// uses it without a running guard.
func ReadIncidents(dataDir string) ([]Incident, error) {
	dir := filepath.Join(dataDir, "guard", "incidents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if fsx.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Incident
	for _, ent := range entries {
		if !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		var inc Incident
		if err := fsx.ReadJSON(filepath.Join(dir, ent.Name()), &inc); err != nil {
			continue
		}
		out = append(out, inc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, nil
}

// Dismiss marks an incident as seen. It stays on disk: an incident is a
// record, and the panel hides what is dismissed.
func (g *Guard) Dismiss(id string) error {
	if id == "" || strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return errors.New("dismiss: not an incident id")
	}
	path := filepath.Join(g.incidentsDir(), id+".json")
	var inc Incident
	if err := fsx.ReadJSON(path, &inc); err != nil {
		return err
	}
	inc.Dismissed = true
	if err := fsx.WriteJSON(path, inc, 0o644); err != nil {
		return err
	}
	g.emit(events.Event{Type: "guard.dismissed", Subject: inc.Domain, Actor: "operator", Data: map[string]any{"id": id}})
	return nil
}

func (g *Guard) emit(e events.Event) {
	if g.opts.Bus != nil {
		g.opts.Bus.Emit(e)
	}
}

// Sweep is one pass: read the table, attribute, judge, act. It returns the
// incidents it recorded, which is what the tests and the panel look at.
func (g *Guard) Sweep() ([]Incident, error) {
	procs, err := g.reader.Processes()
	if err != nil {
		return nil, err
	}
	byPID := make(map[int]*Process, len(procs))
	for i := range procs {
		byPID[procs[i].PID] = &procs[i]
	}
	var sites []SiteInfo
	if g.opts.Sites != nil {
		sites = g.opts.Sites()
	}
	var recorded []Incident
	claimed := map[int]bool{}
	agents := map[int]bool{}
	if g.opts.Agents != nil {
		for _, pid := range g.opts.Agents() {
			agents[pid] = true
		}
	}
	for _, site := range sites {
		root := g.rootPID(site.Domain)
		tree := g.attribute(site, root, procs, byPID, claimed)
		recorded = append(recorded, g.judgeLogs(site, tree, byPID)...)
		g.measureUnits(site.Domain, tree, procs, byPID)
		if len(tree) == 0 {
			continue
		}
		g.reader.fillListening(tree)
		view := View{Site: site, SitesDir: g.opts.SitesDir, PortRange: g.opts.PortRange}
		view.Tree = make([]Process, len(tree))
		for i, p := range tree {
			view.Tree[i] = *p
		}
		disabled := false
		killed := map[int]bool{}
		for _, p := range tree {
			if killed[p.PID] || agents[p.PID] || descendsAny(p, agents, byPID) {
				continue
			}
			view.Proc = *p
			view.Root = p.PID == root
			for _, rule := range rules {
				where, ok := rule.Match(view)
				if !ok {
					continue
				}
				key := site.Domain + "|" + rule.Key + "|" + strconv.Itoa(p.PID)
				if g.recently(key) {
					continue
				}
				inc := g.act(site, rule, *p, where, byPID, &disabled, killed)
				recorded = append(recorded, inc)
				if rule.Level == LevelIntrusion {
					// One intrusion is enough: the tree below it is dead
					// and the site is off, so nothing else needs judging.
					break
				}
			}
		}
	}
	g.huntBrowsers(procs, byPID, claimed)
	return recorded, nil
}

// judgeLogs reads what the site's processes wrote since the last sweep.
// A log intrusion has no process to point at: the site is disabled and
// every process attributed to it is killed, which is what the line says
// someone made it do.
func (g *Guard) judgeLogs(site SiteInfo, tree []*Process, byPID map[int]*Process) []Incident {
	var out []Incident
	disabled := false
	killed := map[int]bool{}
	for _, path := range site.Logs {
		for _, line := range g.newLines(path) {
			r, where, ok := MatchLog(line, site.Home, site.Dir)
			if !ok {
				continue
			}
			key := site.Domain + "|log|" + r.Key + "|" + where
			if g.recently(key) {
				continue
			}
			rule := Rule{Key: r.Key, Level: r.Level, Description: r.Description}
			excerpt := line
			if len(excerpt) > 500 {
				excerpt = excerpt[:500] + "…"
			}
			p := Process{Cmdline: []string{excerpt}}
			if rule.Level == LevelIntrusion {
				for _, t := range tree {
					if !killed[t.PID] {
						g.killTree(t.PID, byPID, killed)
					}
				}
			}
			inc := g.act(site, rule, p, "log "+filepathBase(path)+": "+where, byPID, &disabled, killed)
			out = append(out, inc)
			if rule.Level == LevelIntrusion {
				return out
			}
		}
	}
	return out
}

func filepathBase(p string) string { return filepath.Base(p) }

// rootPID reads the instance pid the engine recorded for a domain.
func (g *Guard) rootPID(domain string) int {
	var st runtime.State
	if err := fsx.ReadJSON(filepath.Join(g.opts.DataDir, "state", domain+".json"), &st); err != nil {
		return 0
	}
	return st.PID
}

// attribute collects a site's processes: the instance and everything below
// it, everything in its process group (a daemon that double-forked away
// from the tree keeps the group the runtime gave the instance), and
// anything whose working directory is inside the site folder, which is how
// workers and build steps are found without the engine telling us.
func (g *Guard) attribute(site SiteInfo, root int, procs []Process, byPID map[int]*Process, claimed map[int]bool) []*Process {
	var out []*Process
	take := func(p *Process) {
		if !claimed[p.PID] {
			claimed[p.PID] = true
			out = append(out, p)
		}
	}
	dir := filepath.Clean(site.Dir)
	for i := range procs {
		p := &procs[i]
		if p.PID == os.Getpid() {
			continue
		}
		if root > 0 && (p.PID == root || p.PGID == root || descends(p, root, byPID)) {
			take(p)
			continue
		}
		if dir != "" && dir != "." && (p.Cwd == dir || strings.HasPrefix(p.Cwd, dir+string(filepath.Separator))) {
			take(p)
		}
	}
	return out
}

func descendsAny(p *Process, roots map[int]bool, byPID map[int]*Process) bool {
	if len(roots) == 0 {
		return false
	}
	for depth := 0; depth < 64 && p != nil; depth++ {
		if roots[p.PPID] {
			return true
		}
		if p.PPID <= 1 {
			return false
		}
		p = byPID[p.PPID]
	}
	return false
}

func descends(p *Process, root int, byPID map[int]*Process) bool {
	for depth := 0; depth < 64 && p != nil; depth++ {
		if p.PPID == root {
			return true
		}
		if p.PPID <= 1 {
			return false
		}
		p = byPID[p.PPID]
	}
	return false
}

func (g *Guard) recently(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if at, ok := g.seen[key]; ok && now.Sub(at) < time.Hour {
		return true
	}
	for k, at := range g.seen {
		if now.Sub(at) > 2*time.Hour {
			delete(g.seen, k)
		}
	}
	g.seen[key] = now
	return false
}

// act records the incident and, for an intrusion, kills the tree and
// disables the site. The site is disabled before the kill: if the kill
// fails (a process we may not signal), the manifest still says stopped and
// the engine's own stop path gets a turn.
func (g *Guard) act(site SiteInfo, rule Rule, p Process, where string, byPID map[int]*Process, disabled *bool, killed map[int]bool) Incident {
	now := g.now()
	inc := Incident{
		ID:      now.UTC().Format("20060102T150405.000000000") + "-" + rule.Key + "-" + strconv.Itoa(p.PID),
		At:      now,
		Domain:  site.Domain,
		Rule:    rule.Key,
		Level:   rule.Level,
		Where:   where,
		PID:     p.PID,
		Cmdline: strings.Join(p.Cmdline, " "),
		Action:  "reported",
	}
	level := "warn"
	typ := "guard.suspicious"
	if rule.Level == LevelIntrusion {
		level = "error"
		typ = "guard.intrusion"
		var actions []string
		if !*disabled {
			if err := g.disable(site); err != nil {
				actions = append(actions, "disable failed: "+err.Error())
			} else {
				actions = append(actions, "site disabled")
			}
			*disabled = true
		}
		n, failed := g.killTree(p.PID, byPID, killed)
		actions = append(actions, fmt.Sprintf("killed %d processes", n))
		if failed > 0 {
			actions = append(actions, fmt.Sprintf("%d could not be killed", failed))
		}
		inc.Action = strings.Join(actions, "; ")
	}
	if err := fsx.WriteJSON(filepath.Join(g.incidentsDir(), inc.ID+".json"), inc, 0o644); err != nil {
		inc.Action += "; incident not written: " + err.Error()
	}
	g.emit(events.Event{Type: typ, Subject: site.Domain, Actor: "guard", Level: level,
		Message: rule.Description + ": " + where,
		Data:    map[string]any{"id": inc.ID, "rule": rule.Key, "pid": p.PID, "action": inc.Action}})
	// Only after acting: the rule decides and cuts, and only then may a
	// model be asked to explain. The other way round, a turn takes a
	// minute and the theft takes milliseconds.
	if rule.Level == LevelIntrusion && g.opts.Contained != nil {
		go g.opts.Contained(inc)
	}
	return inc
}

func (g *Guard) disable(site SiteInfo) error {
	m, err := manifest.Load(site.Dir)
	if err != nil {
		return err
	}
	if m.Disabled {
		return nil
	}
	m.Disabled = true
	return manifest.Save(site.Dir, m)
}

// killTree kills pid and everything below it, leaves first so nothing
// re-spawns while its parent is still alive to notice. done records what
// was signalled so the same sweep does not judge or kill it twice.
func (g *Guard) killTree(pid int, byPID map[int]*Process, done map[int]bool) (killed, failed int) {
	// Never below 2: the children of 0 are init and kthreadd, and a walk
	// from there is every process on the machine. A finding from a log
	// line has no pid, and must not become that.
	if pid < 2 {
		return 0, 0
	}
	children := map[int][]int{}
	for _, p := range byPID {
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	var walk func(int)
	walk = func(id int) {
		for _, c := range children[id] {
			walk(c)
		}
		if done[id] {
			return
		}
		done[id] = true
		if err := g.kill(id); err != nil && !errors.Is(err, syscall.ESRCH) {
			failed++
			return
		}
		killed++
	}
	walk(pid)
	return
}

func isBrowser(p *Process) bool {
	hay := strings.ToLower(p.Comm + " " + filepath.Base(p.Exe) + " " + firstArg(p.Cmdline))
	for _, sig := range []string{"chrome", "chromium", "headless_shell", "headless-shell", "puppeteer", "playwright", "firefox", "webkit"} {
		if strings.Contains(hay, sig) {
			return true
		}
	}
	return false
}
