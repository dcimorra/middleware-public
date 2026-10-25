package cgroups

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The split is written against a folder shaped like the unit's cgroup:
// what can be checked without a delegated cgroup is what is written where.
func TestTheAgentsBranchGetsItsWeightAndCeiling(t *testing.T) {
	defer func(r string) { Root = r }(Root)
	Root = t.TempDir()
	unit := filepath.Join(Root, "system.slice", "middleware.service")
	os.MkdirAll(filepath.Join(unit, "house"), 0o755)
	os.WriteFile(filepath.Join(unit, "cgroup.subtree_control"), nil, 0o644)
	tree, err := Setup("/system.slice/middleware.service/house", Limits{CPUWeight: 10, CPUCores: 2, MemoryHigh: 3 << 30, MemoryMax: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	read := func(f string) string {
		b, _ := os.ReadFile(filepath.Join(unit, f))
		return strings.TrimSpace(string(b))
	}
	for f, want := range map[string]string{
		"cgroup.subtree_control": "+cpu +memory", "agents/cpu.weight": "10", "agents/cpu.max": "200000 100000",
		"agents/memory.high": "3221225472", "agents/memory.max": "4294967296",
	} {
		if got := read(f); got != want {
			t.Errorf("%s = %q, want %q", f, got, want)
		}
	}
	attr, release, err := tree.AgentsAttr()
	if err != nil || attr == nil || !attr.UseCgroupFD {
		t.Fatalf("attr %+v %v", attr, err)
	}
	release()
}

func TestWithoutDelegationNothingIsTouched(t *testing.T) {
	defer func(r string) { Root = r }(Root)
	Root = t.TempDir()
	if _, err := Setup("/user.slice/session-1.scope", DefaultLimits()); err != ErrNotDelegated {
		t.Fatalf("%v", err)
	}
	var none *Tree
	if attr, _, err := none.AgentsAttr(); attr != nil || err != nil {
		t.Fatal("a nil tree must give no attributes")
	}
}

// With a systemd older than 254 (no DelegateSubgroup) the supervisor
// starts in the unit's own cgroup: it makes house/, moves every process
// there, and then splits as usual.
func TestWithoutDelegateSubgroupTheHouseMovesItselfFirst(t *testing.T) {
	defer func(r string) { Root = r }(Root)
	defer func(f func(string, string) error) { movePID = f }(movePID)
	Root = t.TempDir()
	unit := filepath.Join(Root, "system.slice", "middleware.service")
	os.MkdirAll(unit, 0o755)
	os.WriteFile(filepath.Join(unit, "cgroup.subtree_control"), nil, 0o644)
	os.WriteFile(filepath.Join(unit, "cgroup.procs"), []byte("100\n200\n"), 0o644)
	var moved []string
	// What the kernel does: the process leaves the unit's list.
	movePID = func(cgroup, pid string) error {
		if filepath.Base(cgroup) != HouseName {
			t.Fatalf("moved into %s", cgroup)
		}
		moved = append(moved, pid)
		raw, _ := os.ReadFile(filepath.Join(unit, "cgroup.procs"))
		rest := strings.Replace(string(raw), pid+"\n", "", 1)
		return os.WriteFile(filepath.Join(unit, "cgroup.procs"), []byte(rest), 0o644)
	}
	if _, err := Setup("/system.slice/middleware.service", Limits{CPUWeight: 10}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(moved, ",") != "100,200" {
		t.Fatalf("moved %v", moved)
	}
	if b, _ := os.ReadFile(filepath.Join(unit, "agents", "cpu.weight")); string(b) != "10" {
		t.Fatalf("agents/ was not set up after the move: %q", b)
	}
	// A unit whose cgroup is not ours (no delegation) is left alone.
	os.Chmod(filepath.Join(unit, "cgroup.subtree_control"), 0o444)
	if os.Geteuid() != 0 {
		if _, err := Setup("/system.slice/middleware.service", Limits{}); err != ErrNotDelegated {
			t.Fatalf("not delegated: %v", err)
		}
	}
}
