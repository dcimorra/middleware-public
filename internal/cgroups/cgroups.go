// Package cgroups keeps the agents' work from competing with the sites.
//
// On 16 October 2026 V1's machine ran out of air with three headless
// Chromiums at once, started by one site's conversations to test its games:
// each rendered WebGL in software and its GPU process ate one to two cores.
// The site was in a container with limits; its conversations were not: they
// ran on the host as children of the supervisor, and what they started
// competed with the sites as an equal. The limits were on the site, and the
// cost was born in its conversation.
//
// So the unit's cgroup is split in two siblings:
//
//	middleware.service/
//	├── house/   the supervisor, the sites and the tasks
//	└── agents/  every agent turn and whatever it starts
//
// and agents get a low CPU weight and a ceiling. A weight is not a cap: with
// the machine idle they use every core as before, and only when a site asks
// for CPU does the kernel serve it first. Turns start inside agents/
// directly (clone into the cgroup), so nothing a turn starts is ever
// briefly outside it.
//
// It needs Delegate=cpu memory and DelegateSubgroup=house in the unit
// (systemd 254 or newer): without delegation the cgroup is root's and the
// supervisor cannot create a folder in it. Without it everything runs as
// before and the start says so once.
package cgroups

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// The branches. House must match the unit's DelegateSubgroup=: it is where
// systemd puts the supervisor at start.
const (
	HouseName  = "house"
	AgentsName = "agents"
)

// Limits are what the agents' branch gets.
type Limits struct {
	// CPUWeight against the house's 100: with both asking, agents get a
	// small share.
	CPUWeight int
	// CPUCores caps agents in cores. The weight shares under contention,
	// but the scheduler takes a while to turn and a site feels it as
	// latency spikes; with a core that is never theirs, it does not.
	CPUCores int
	// MemoryHigh is where the kernel starts squeezing (reclaiming,
	// slowing) without killing anyone.
	MemoryHigh int64
	// MemoryMax is the net: past it the kernel kills, inside this branch,
	// whatever holds the most, which is a browser. On a machine without
	// swap the alternative is a site dying.
	MemoryMax int64
}

// DefaultLimits are sized from the machine: one core always left to the
// sites, and a third of the memory as the ceiling.
func DefaultLimits() Limits {
	cores := runtime.NumCPU() - 1
	if cores < 1 {
		cores = 1
	}
	total := totalMemory()
	l := Limits{CPUWeight: 10, CPUCores: cores}
	if total > 0 {
		l.MemoryMax = total / 3
		l.MemoryHigh = total / 4
	}
	return l
}

// ErrNotDelegated is a unit without delegation.
var ErrNotDelegated = errors.New("the supervisor's cgroup is not delegated (the unit needs Delegate=cpu memory and DelegateSubgroup=house, systemd 254 or newer); agent turns share the sites' CPU and memory")

// Tree is the split, once made.
type Tree struct {
	agents string
}

// Root is where the unified hierarchy is mounted; tests use a folder.
var Root = "/sys/fs/cgroup"

// Setup splits the unit's cgroup. self is this process's cgroup path as
// /proc/self/cgroup says ("/system.slice/middleware.service/house").
func Setup(self string, l Limits) (*Tree, error) {
	if filepath.Base(self) != HouseName {
		return nil, ErrNotDelegated
	}
	unit := filepath.Join(Root, filepath.Dir(self))
	if syscall.Access(filepath.Join(unit, "cgroup.subtree_control"), 2) != nil {
		return nil, ErrNotDelegated
	}
	// Controllers have to be enabled on the parent for the children to
	// have the files; "+cpu +memory" on a cgroup that already has them is
	// a no-op.
	if err := os.WriteFile(filepath.Join(unit, "cgroup.subtree_control"), []byte("+cpu +memory"), 0o644); err != nil {
		return nil, fmt.Errorf("cgroups: enable controllers: %w", err)
	}
	agents := filepath.Join(unit, AgentsName)
	if err := os.MkdirAll(agents, 0o755); err != nil {
		return nil, err
	}
	writes := map[string]string{"cpu.weight": strconv.Itoa(l.CPUWeight)}
	if l.CPUCores > 0 {
		writes["cpu.max"] = strconv.Itoa(l.CPUCores*100000) + " 100000"
	}
	if l.MemoryHigh > 0 {
		writes["memory.high"] = strconv.FormatInt(l.MemoryHigh, 10)
	}
	if l.MemoryMax > 0 {
		writes["memory.max"] = strconv.FormatInt(l.MemoryMax, 10)
	}
	for f, v := range writes {
		if err := os.WriteFile(filepath.Join(agents, f), []byte(v), 0o644); err != nil {
			return nil, fmt.Errorf("cgroups: %s: %w", f, err)
		}
	}
	return &Tree{agents: agents}, nil
}

// SelfPath reads this process's cgroup v2 path.
func SelfPath() string {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(p)
		}
	}
	return ""
}

// AgentsAttr is a SysProcAttr that starts a process inside agents/, or
// nil without a tree. The caller owns the descriptor until Start returns.
func (t *Tree) AgentsAttr() (*syscall.SysProcAttr, func(), error) {
	if t == nil {
		return nil, func() {}, nil
	}
	fd, err := syscall.Open(t.agents, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, func() {}, err
	}
	return &syscall.SysProcAttr{Setpgid: true, UseCgroupFD: true, CgroupFD: fd}, func() { syscall.Close(fd) }, nil
}

// Agents is the agents' branch path, for the panel.
func (t *Tree) Agents() string {
	if t == nil {
		return ""
	}
	return t.agents
}

func totalMemory() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			f := strings.Fields(v)
			if len(f) > 0 {
				kb, _ := strconv.ParseInt(f[0], 10, 64)
				return kb << 10
			}
		}
	}
	return 0
}
