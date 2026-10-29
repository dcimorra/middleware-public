//go:build linux

package jail

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// cloneFlags are the namespaces a jail gets. Not the network's: a site
// listens on its loopback port and the proxy must reach it there.
const cloneFlags = syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS

// self is the binary the parent re-executes as the jail's init. The
// running image, not the path it was started from: an update replaces the
// file on disk, and the init must be the version that wrote the plan.
const self = "/proc/self/exe"

// The capabilities the init holds while it builds the jail.
const (
	capSetPCap  = 8
	capSysAdmin = 21
)

// Wrap makes cmd start inside a jail built from plan. cmd must be built
// (exec.Command) and not started; its Path, Args, Dir, Env, files and the
// SysProcAttr it already has (a process group, a cgroup) are kept, and
// the command runs as pid 2 of its own namespaces under the init.
func Wrap(cmd *exec.Cmd, plan *Plan) error {
	if cmd.Process != nil {
		return errors.New("jail: command already started")
	}
	p := *plan
	if p.Dir == "" {
		p.Dir = cmd.Dir
	}
	enc, err := p.encode()
	if err != nil {
		return err
	}
	args := append([]string{"middleware-jail", Arg, enc, "--", cmd.Path}, cmd.Args[1:]...)
	cmd.Path, cmd.Args = self, args
	attr := cmd.SysProcAttr
	if attr == nil {
		attr = &syscall.SysProcAttr{}
	}
	attr.Cloneflags |= cloneFlags
	uid, gid := os.Getuid(), os.Getgid()
	attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}}
	attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}}
	attr.GidMappingsEnableSetgroups = false
	// The init is this uid, not root, inside: an exec by any other uid in
	// a user namespace drops every capability, and the init needs two to
	// build the jail (mounts and the hostname; emptying the bounding set).
	// Ambient, so they survive its own exec; it clears them before the
	// command starts (lockDown).
	attr.AmbientCaps = []uintptr{capSysAdmin, capSetPCap}
	cmd.SysProcAttr = attr
	return nil
}

var (
	probeOnce sync.Once
	probeErr  error
)

// Available says whether jails can be built on this machine, checked once
// by building one and running `true` in it: a kernel or an AppArmor
// policy that refuses unprivileged user namespaces, or a /proc that
// cannot be mounted fresh (systemd's ProtectKernelTunables), fails here
// rather than at a site's start.
func Available() error {
	probeOnce.Do(func() { probeErr = probe() })
	return probeErr
}

func probe() error {
	bin, err := exec.LookPath("true")
	if err != nil {
		return fmt.Errorf("jail: no `true` to test with: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Dir = "/"
	if err := Wrap(cmd, System("probe")); err != nil {
		return err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if len(out) > 0 {
			return fmt.Errorf("jail: %s", firstLine(out))
		}
		return fmt.Errorf("jail: %w", err)
	}
	return nil
}

func firstLine(b []byte) string {
	for i, c := range b {
		if c == '\n' {
			return string(b[:i])
		}
	}
	return string(b)
}
