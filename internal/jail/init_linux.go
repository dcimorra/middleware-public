//go:build linux

package jail

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

// The init runs before main: the binary was re-executed only to be it,
// and nothing else of the program (flags, settings, a test's TestMain)
// must run first.
func init() {
	if len(os.Args) > 1 && os.Args[1] == Arg {
		os.Exit(initMain(os.Args[2:]))
	}
}

// exitSetup is the init's status when the jail could not be built: the
// command never ran. 125, as env(1) and docker do for their own failure.
const exitSetup = 125

func initMain(args []string) int {
	if len(args) < 3 || args[1] != "--" {
		fmt.Fprintln(os.Stderr, "jail: usage: "+Arg+" <plan> -- command...")
		return exitSetup
	}
	plan, err := decode(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "jail: bad plan:", err)
		return exitSetup
	}
	// Every step below changes this thread's or this process's state, and
	// the capabilities dropped at the end are a thread's: the command is
	// started from this same thread so it inherits exactly what is left.
	runtime.LockOSThread()
	if err := build(plan); err != nil {
		fmt.Fprintln(os.Stderr, "jail:", err)
		return exitSetup
	}
	return run(args[2:])
}

// Where the new root is put together, inside the scratch root.
const (
	host  = "/host" // the machine's root, until the end of build
	fresh = "/root" // the jail's root, being filled
)

// build makes the jail's root and moves into it. Two pivots: the first
// puts a scratch tmpfs at / with the machine under /host, so every source
// is read at /host/<path> and nothing the plan names is hidden under a
// mount made here (a site folder under /tmp, as in the tests, would be
// under the /tmp of the second step otherwise); the second makes the
// filled root the root and drops the machine with the scratch.
func build(p *Plan) error {
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("making mounts private: %w", err)
	}
	scratch := "/tmp"
	if err := syscall.Mount("tmpfs", scratch, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=0700"); err != nil {
		return fmt.Errorf("scratch tmpfs: %w", err)
	}
	if err := os.Mkdir(scratch+host, 0o700); err != nil {
		return err
	}
	if err := pivot(scratch, scratch+host); err != nil {
		return err
	}
	if err := os.Mkdir(fresh, 0o755); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", fresh, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=0755"); err != nil {
		return fmt.Errorf("root tmpfs: %w", err)
	}
	// The jail's own /tmp and /dev first, the plan's mounts over them: a
	// folder the plan names under /tmp (a test's root) stays visible.
	if err := devices(); err != nil {
		return fmt.Errorf("/dev: %w", err)
	}
	if err := tmpfs(fresh+"/tmp", "mode=1777"); err != nil {
		return fmt.Errorf("/tmp: %w", err)
	}
	for _, m := range p.sorted() {
		if err := place(m); err != nil {
			return fmt.Errorf("%s: %w", m.Dst, err)
		}
	}
	// Its own pid namespace's: the one process tree it can name is its own.
	if err := os.MkdirAll(fresh+"/proc", 0o555); err != nil {
		return err
	}
	if err := syscall.Mount("proc", fresh+"/proc", "proc", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("/proc: %w", err)
	}
	if p.Hostname != "" {
		_ = syscall.Sethostname([]byte(p.Hostname))
	}
	if err := os.Mkdir(fresh+"/.old", 0o700); err != nil {
		return err
	}
	if err := pivot(fresh, fresh+"/.old"); err != nil {
		return err
	}
	if err := syscall.Unmount("/.old", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("leaving the machine's root: %w", err)
	}
	_ = os.Remove("/.old")
	// The root itself is read-only: what it holds is mount points.
	if err := syscall.Mount("", "/", "", syscall.MS_REMOUNT|syscall.MS_BIND|syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV, ""); err != nil {
		return fmt.Errorf("read-only root: %w", err)
	}
	dir := p.Dir
	if dir == "" {
		dir = "/"
	}
	if err := os.Chdir(dir); err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	return nil
}

func pivot(newRoot, putOld string) error {
	if err := syscall.PivotRoot(newRoot, putOld); err != nil {
		return fmt.Errorf("pivot_root %s: %w", newRoot, err)
	}
	return os.Chdir("/")
}

// place puts one mount of the plan into the new root.
func place(m Mount) error {
	dst := fresh + m.Dst
	if m.Link != "" {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.Symlink(m.Link, dst)
	}
	src := host + m.Src
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if st.IsDir() {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
	} else if _, err := os.Lstat(dst); err != nil {
		// A file under a folder already mounted is already there (and
		// read-only, so it could not be made); otherwise an empty file is
		// its mount point.
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		f.Close()
	}
	if err := syscall.Mount(src, dst, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return err
	}
	flags := uintptr(syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_NOSUID | syscall.MS_NODEV)
	if !m.Write {
		flags |= syscall.MS_RDONLY
	}
	return syscall.Mount("", dst, "", flags|kept(dst), "")
}

// kept are the flags a bind remount must repeat from the mount it copies:
// in a user namespace the kernel refuses a remount that would clear one
// set by the namespace the mount came from (noexec, an atime mode).
func kept(path string) uintptr {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0
	}
	// The ST_ bits statfs reports are the MS_ bits mount takes.
	const keep = syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC |
		syscall.MS_NOATIME | syscall.MS_NODIRATIME | syscall.MS_RELATIME
	return uintptr(s.Flags) & keep
}

func tmpfs(path, opts string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	return syscall.Mount("tmpfs", path, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, opts)
}

// devices is a /dev of the few nodes programs open by name, each the
// machine's own bound in, plus the links to the descriptors and a /dev/shm
// for the browsers an agent drives.
func devices() error {
	dev := fresh + "/dev"
	if err := os.MkdirAll(dev, 0o755); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", dev, "tmpfs", syscall.MS_NOSUID, "mode=0755"); err != nil {
		return err
	}
	for _, name := range []string{"null", "zero", "full", "random", "urandom", "tty"} {
		f, err := os.OpenFile(filepath.Join(dev, name), os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			return err
		}
		f.Close()
		if err := syscall.Mount(host+"/dev/"+name, filepath.Join(dev, name), "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	for name, target := range map[string]string{"fd": "/proc/self/fd", "stdin": "/proc/self/fd/0", "stdout": "/proc/self/fd/1", "stderr": "/proc/self/fd/2"} {
		if err := os.Symlink(target, filepath.Join(dev, name)); err != nil {
			return err
		}
	}
	// A terminal of its own when the kernel gives one; a command without
	// a tty does not need it, so its absence is not a failure.
	if err := os.Mkdir(filepath.Join(dev, "pts"), 0o755); err == nil {
		if syscall.Mount("devpts", filepath.Join(dev, "pts"), "devpts", syscall.MS_NOSUID|syscall.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620") == nil {
			_ = os.Symlink("pts/ptmx", filepath.Join(dev, "ptmx"))
		}
	}
	return tmpfs(filepath.Join(dev, "shm"), "mode=1777")
}

// Signals the init passes on to the command's group. The supervisor
// signals the init's group from outside; the command is in a group of its
// own inside, so each signal reaches it once, through here. SIGKILL needs
// no passing: the init dies, and with pid 1 the whole namespace.
var forwarded = []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGWINCH}

// run starts the command and stays as its pid 1: passes signals on, reaps
// whatever is orphaned to it, and exits with the command's status. When it
// exits the kernel ends every process left in the namespace, which is the
// group kill the runtimes did before, done by the kernel.
func run(argv []string) int {
	if err := lockDown(); err != nil {
		fmt.Fprintln(os.Stderr, "jail:", err)
		return exitSetup
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, forwarded...)
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "jail:", err)
		return 127
	}
	child := cmd.Process.Pid
	go func() {
		for s := range sigs {
			_ = syscall.Kill(-child, s.(syscall.Signal))
		}
	}()
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return 1
		}
		if pid != child {
			continue
		}
		switch {
		case ws.Exited():
			return ws.ExitStatus()
		case ws.Signaled():
			return 128 + int(ws.Signal())
		}
	}
}

// Capabilities, numbered by the kernel; the last one this binary knows of
// is far below 64, and a number the kernel does not know is refused with
// EINVAL, which ends the loop.
const (
	prSetNoNewPrivs = 38
	prCapBSetDrop   = 24
	prCapAmbient    = 47
	prCapAmbientAll = 4 // PR_CAP_AMBIENT_CLEAR_ALL
	linuxCapV3      = 0x20080522
)

// lockDown leaves the command nothing the init had to build the jail: no
// capability can be gained again (the bounding set is emptied, which also
// covers a jail whose user is root inside because it is root outside) and
// no exec can grant privileges (no_new_privs, which also turns setuid bits
// and file capabilities off).
func lockDown() error {
	for c := uintptr(0); c < 64; c++ {
		if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prCapBSetDrop, c, 0); e == syscall.EINVAL {
			break
		} else if e != 0 {
			return fmt.Errorf("dropping capability %d: %w", c, e)
		}
	}
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientAll, 0); e != 0 {
		return fmt.Errorf("clearing ambient capabilities: %w", e)
	}
	// And the thread's own sets, inheritable included: for a jail whose
	// user is root inside, exec hands the inheritable set on whatever the
	// bounding set says.
	hdr := struct{ version, pid uint32 }{linuxCapV3, 0}
	var data [2]struct{ effective, permitted, inheritable uint32 }
	if _, _, e := syscall.RawSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); e != 0 {
		return fmt.Errorf("clearing capabilities: %w", e)
	}
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); e != 0 {
		return fmt.Errorf("no_new_privs: %w", e)
	}
	return nil
}
