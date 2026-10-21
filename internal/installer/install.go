package installer

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/dcimorra/middleware-public/internal/fsx"
)

// Paths are the system directories the installer writes to. Variables so
// tests can point them at t.TempDir() and the real machine is never touched.
type Paths struct {
	SystemdDir  string
	TmpfilesDir string
	SysctlDir   string
}

// SystemPaths are where a Linux distribution expects these files.
func SystemPaths() Paths {
	return Paths{SystemdDir: "/etc/systemd/system", TmpfilesDir: "/etc/tmpfiles.d", SysctlDir: "/etc/sysctl.d"}
}

// Installer runs the steps. Zero values are filled by New.
type Installer struct {
	Exec   Exec
	Params Params
	Paths  Paths
	// Out receives one line per step.
	Out io.Writer
	// Source is the executable to copy into Params.Binary; defaults to the
	// running one.
	Source string
	// NoStart renders and enables but never starts or restarts a unit.
	NoStart bool
	// KeepBinary leaves Params.Binary in place on uninstall.
	KeepBinary bool
	// OperatorExists says whether `middleware setup` already ran. The auth
	// package owns the file; the installer only needs a yes or no.
	OperatorExists func(root string) bool
	// Live says Out is a terminal, where progress rewrites one line.
	Live bool
	// FindBrowser looks for a headless Chromium; nil is shots.FindIn.
	FindBrowser func(root string) string
	// BrowserPlatform overrides the Chrome for Testing platform (tests).
	BrowserPlatform string

	// setupOutput is what `middleware setup` printed, shown after the step
	// lines so the one-time code is the last thing on screen.
	setupOutput string
}

// New returns an installer against the real machine for the current user.
func New(p Params) *Installer {
	if p.User == "" {
		if u, err := user.Current(); err == nil {
			p.User = u.Username
		}
	}
	return &Installer{Exec: System{}, Params: p, Paths: SystemPaths(), Out: os.Stdout}
}

// Step is one re-runnable unit of work. changed is false when the machine
// was already in the wanted state, so a re-run prints "unchanged" lines and
// touches nothing.
type Step struct {
	Name string
	Run  func() (changed bool, detail string, err error)
}

// Report is what a run produced, for callers that want more than the lines.
type Report struct {
	Changed []string
	Failed  []string
}

func (in *Installer) defaults() error {
	if in.Exec == nil {
		in.Exec = System{}
	}
	if in.Out == nil {
		in.Out = io.Discard
	}
	if in.Paths == (Paths{}) {
		in.Paths = SystemPaths()
	}
	if in.Source == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("cannot find own executable: %w", err)
		}
		in.Source = exe
	}
	if in.OperatorExists == nil {
		in.OperatorExists = defaultOperatorExists
	}
	return in.Params.Validate()
}

// defaultOperatorExists says the operator's password is set.
func defaultOperatorExists(root string) bool {
	// Where the auth package keeps the operator's password. The installer
	// looked for data/auth.json, which nothing writes: every re-run took
	// the house for new and printed a setup code, which can set a new
	// password.
	b, err := os.ReadFile(filepath.Join(root, "data", "auth", "password"))
	return err == nil && len(bytes.TrimSpace(b)) > 0
}

func (in *Installer) runSteps(steps []Step) (Report, error) {
	var rep Report
	for _, s := range steps {
		changed, detail, err := s.Run()
		switch {
		case err != nil:
			rep.Failed = append(rep.Failed, s.Name)
			fmt.Fprintf(in.Out, "✗ %-26s %v\n", s.Name, err)
			return rep, fmt.Errorf("%s: %w", s.Name, err)
		case changed:
			rep.Changed = append(rep.Changed, s.Name)
			fmt.Fprintf(in.Out, "✓ %-26s %s\n", s.Name, detail)
		default:
			if detail == "" {
				detail = "unchanged"
			}
			fmt.Fprintf(in.Out, "✓ %-26s %s\n", s.Name, detail)
		}
	}
	return rep, nil
}

// Install runs every step in order and stops at the first failure, which is
// the safest thing an installer can do: a later step that assumes an
// earlier one would leave a half machine that looks whole.
func (in *Installer) Install() (Report, error) {
	if err := in.defaults(); err != nil {
		return Report{}, err
	}
	if os.Geteuid() == 0 && in.Params.User == "root" {
		return Report{}, errors.New("refusing to install for root: run as the user that will operate the panel (sudo is used where needed)")
	}
	var unitsChanged, binaryChanged bool
	steps := []Step{
		{"package manager", in.stepPackageManager},
		{"git, curl and wireguard-tools", in.stepPackages},
		{"system group", in.stepGroup},
		{"user in group", in.stepUserInGroup},
		{"root directory", in.stepRootDir},
		{"headless browser", in.stepBrowser},
		{"binary", func() (bool, string, error) {
			c, d, err := in.stepBinary()
			binaryChanged = c
			return c, d, err
		}},
		{"units and config", func() (bool, string, error) {
			c, d, err := in.stepFiles()
			unitsChanged = c
			return c, d, err
		}},
		{"sysctl", in.stepSysctl},
		{"tmpfiles", in.stepTmpfiles},
		{"daemon-reload", func() (bool, string, error) {
			if !unitsChanged {
				return false, "", nil
			}
			_, err := in.Exec.Sudo("systemctl", "daemon-reload")
			return true, "reloaded", err
		}},
		{"enable units", in.stepEnable},
		{"start units", func() (bool, string, error) { return in.stepStart(unitsChanged || binaryChanged) }},
		{"operator", in.stepSetup},
	}
	rep, err := in.runSteps(steps)
	if in.setupOutput != "" {
		// setup's own lines say where the panel is: https://<domain>, or
		// the loopback address and the SSH tunnel to reach it.
		fmt.Fprintf(in.Out, "\n%s", in.setupOutput)
	}
	return rep, err
}

// Uninstall stops and removes the units and the system files. It never
// touches the root directory: the sites are the operator's work and a
// command that deletes them by default is a command that will be run once
// by mistake.
func (in *Installer) Uninstall() (Report, error) {
	if err := in.defaults(); err != nil {
		return Report{}, err
	}
	units := in.unitNames()
	steps := []Step{
		{"stop units", func() (bool, string, error) {
			changed := false
			for _, u := range units {
				if !in.isActive(u) {
					continue
				}
				if _, err := in.Exec.Sudo("systemctl", "stop", u); err != nil {
					return changed, "", err
				}
				changed = true
			}
			return changed, "stopped", nil
		}},
		{"disable units", func() (bool, string, error) {
			changed := false
			for _, u := range units {
				if !in.isEnabled(u) {
					continue
				}
				if _, err := in.Exec.Sudo("systemctl", "disable", u); err != nil {
					return changed, "", err
				}
				changed = true
			}
			return changed, "disabled", nil
		}},
		{"remove files", func() (bool, string, error) {
			changed := false
			for _, p := range in.filePaths() {
				if !fsx.Exists(p) {
					continue
				}
				if err := in.Exec.RemoveRoot(p); err != nil {
					return changed, "", err
				}
				changed = true
			}
			if changed {
				if _, err := in.Exec.Sudo("systemctl", "daemon-reload"); err != nil {
					return true, "", err
				}
			}
			return changed, "removed", nil
		}},
		{"binary", func() (bool, string, error) {
			if in.KeepBinary || !fsx.Exists(in.Params.Binary) {
				return false, "kept", nil
			}
			return true, "removed " + in.Params.Binary, in.Exec.RemoveRoot(in.Params.Binary)
		}},
	}
	rep, err := in.runSteps(steps)
	fmt.Fprintf(in.Out, "\nKept %s with every site, task and data file in it. Remove it yourself if that is what you want.\n", in.Params.Root)
	return rep, err
}

// unitNames lists the units in the order they are started.
func (in *Installer) unitNames() []string {
	units := []string{SocketUnit}
	if in.Params.Helper {
		units = append(units, HelperUnit)
	}
	return append(units, ServiceUnit)
}

// filePaths is every file the installer owns outside the root.
func (in *Installer) filePaths() []string {
	return []string{
		filepath.Join(in.Paths.SystemdDir, ServiceUnit),
		filepath.Join(in.Paths.SystemdDir, SocketUnit),
		filepath.Join(in.Paths.SystemdDir, HelperUnit),
		filepath.Join(in.Paths.TmpfilesDir, TmpfilesIn),
		filepath.Join(in.Paths.SysctlDir, SysctlIn),
	}
}

func (in *Installer) pathFor(name string) string {
	switch name {
	case TmpfilesIn:
		return filepath.Join(in.Paths.TmpfilesDir, name)
	case SysctlIn:
		return filepath.Join(in.Paths.SysctlDir, name)
	default:
		return filepath.Join(in.Paths.SystemdDir, name)
	}
}

// packageManagers in the order they are looked for. apt-get before apt
// because apt warns about its CLI being unstable when scripted.
var packageManagers = []struct {
	bin     string
	install []string
}{
	{"apt-get", []string{"apt-get", "install", "-y", "-q"}},
	{"dnf", []string{"dnf", "install", "-y", "-q"}},
	{"apk", []string{"apk", "add", "--no-cache"}},
	{"pacman", []string{"pacman", "-S", "--noconfirm", "--needed"}},
	{"zypper", []string{"zypper", "--non-interactive", "install"}},
}

func (in *Installer) packageManager() ([]string, string) {
	for _, pm := range packageManagers {
		if _, err := in.Exec.LookPath(pm.bin); err == nil {
			return pm.install, pm.bin
		}
	}
	return nil, ""
}

func (in *Installer) stepPackageManager() (bool, string, error) {
	_, name := in.packageManager()
	if name == "" {
		return false, "none found (apt, dnf, apk, pacman, zypper); packages will not be installed", nil
	}
	return false, name, nil
}

func (in *Installer) stepPackages() (bool, string, error) {
	var missing []string
	// Binary → package: wg comes from wireguard-tools, so the vpn module
	// finds it there the day it is enabled instead of asking the operator
	// to apt-get something by hand.
	for _, p := range []struct{ bin, pkg string }{{"git", "git"}, {"curl", "curl"}, {"wg", "wireguard-tools"}} {
		if _, err := in.Exec.LookPath(p.bin); err != nil {
			missing = append(missing, p.pkg)
		}
	}
	if len(missing) == 0 {
		return false, "present", nil
	}
	cmd, name := in.packageManager()
	if cmd == nil {
		return false, "", fmt.Errorf("missing %s and no package manager to install them", strings.Join(missing, ", "))
	}
	args := append(append([]string{}, cmd[1:]...), missing...)
	if _, err := in.Exec.Sudo(cmd[0], args...); err != nil {
		return false, "", err
	}
	return true, fmt.Sprintf("installed %s with %s", strings.Join(missing, ", "), name), nil
}

func (in *Installer) stepGroup() (bool, string, error) {
	if _, err := in.Exec.Run("getent", "group", in.Params.Group); err == nil {
		return false, in.Params.Group + " exists", nil
	}
	if _, err := in.Exec.Sudo("groupadd", "--system", in.Params.Group); err != nil {
		return false, "", err
	}
	return true, "created " + in.Params.Group, nil
}

func (in *Installer) stepUserInGroup() (bool, string, error) {
	out, err := in.Exec.Run("id", "-nG", in.Params.User)
	if err != nil {
		return false, "", fmt.Errorf("user %s: %w", in.Params.User, err)
	}
	for _, g := range strings.Fields(out) {
		if g == in.Params.Group {
			return false, fmt.Sprintf("%s is in %s", in.Params.User, in.Params.Group), nil
		}
	}
	if _, err := in.Exec.Sudo("usermod", "-aG", in.Params.Group, in.Params.User); err != nil {
		return false, "", err
	}
	return true, fmt.Sprintf("added %s to %s (takes effect at next login)", in.Params.User, in.Params.Group), nil
}

var rootSubdirs = []string{"sites", "tasks", "data", "modules"}

func (in *Installer) stepRootDir() (bool, string, error) {
	root := in.Params.Root
	changed := false
	if !fsx.Exists(root) {
		// The parent (/srv) is usually root's. Try as the user first so a
		// root under $HOME needs no sudo at all.
		if err := os.MkdirAll(root, 0o755); err != nil {
			if _, err := in.Exec.Sudo("install", "-d", "-m", "0755", "-o", in.Params.User, "-g", in.Params.Group, root); err != nil {
				return false, "", err
			}
		}
		changed = true
	}
	for _, d := range rootSubdirs {
		p := filepath.Join(root, d)
		if fsx.Exists(p) {
			continue
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			return changed, "", fmt.Errorf("%s: %w (is %s owned by %s?)", p, err, root, in.Params.User)
		}
		changed = true
	}
	if changed {
		return true, "created " + root, nil
	}
	return false, root, nil
}

func fileSum(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

func (in *Installer) stepBinary() (bool, string, error) {
	src, err := fileSum(in.Source)
	if err != nil {
		return false, "", err
	}
	if dst, err := fileSum(in.Params.Binary); err == nil && bytes.Equal(src, dst) {
		return false, in.Params.Binary + " is current", nil
	}
	data, err := os.ReadFile(in.Source)
	if err != nil {
		return false, "", err
	}
	if err := in.Exec.WriteRoot(in.Params.Binary, data, 0o755); err != nil {
		return false, "", err
	}
	return true, "installed " + in.Params.Binary, nil
}

// stepFiles writes every rendered file whose content differs. Comparing
// first is what makes a re-run quiet, and what keeps daemon-reload from
// running when nothing changed.
func (in *Installer) stepFiles() (bool, string, error) {
	files, err := Render(in.Params)
	if err != nil {
		return false, "", err
	}
	var written []string
	for _, name := range []string{ServiceUnit, SocketUnit, HelperUnit, TmpfilesIn, SysctlIn} {
		content, ok := files[name]
		if !ok {
			continue
		}
		p := in.pathFor(name)
		if cur, err := os.ReadFile(p); err == nil && string(cur) == content {
			continue
		}
		if err := in.Exec.WriteRoot(p, []byte(content), 0o644); err != nil {
			return len(written) > 0, "", err
		}
		written = append(written, name)
	}
	if len(written) == 0 {
		return false, "current", nil
	}
	return true, "wrote " + strings.Join(written, ", "), nil
}

func (in *Installer) stepSysctl() (bool, string, error) {
	want := fmt.Sprintf("%d-%d", in.Params.PortsFrom, in.Params.PortsTo)
	if out, err := in.Exec.Run("sysctl", "-n", "net.ipv4.ip_local_reserved_ports"); err == nil && strings.TrimSpace(out) == want {
		return false, "ports " + want + " reserved", nil
	}
	if _, err := in.Exec.Sudo("sysctl", "-q", "-p", in.pathFor(SysctlIn)); err != nil {
		return false, "", err
	}
	return true, "reserved ports " + want, nil
}

func (in *Installer) stepTmpfiles() (bool, string, error) {
	if fsx.Exists(in.Params.RunDir) {
		return false, in.Params.RunDir + " exists", nil
	}
	if _, err := in.Exec.Sudo("systemd-tmpfiles", "--create", in.pathFor(TmpfilesIn)); err != nil {
		return false, "", err
	}
	return true, "created " + in.Params.RunDir, nil
}

func (in *Installer) isEnabled(unit string) bool {
	out, err := in.Exec.Run("systemctl", "is-enabled", unit)
	return err == nil && strings.TrimSpace(out) == "enabled"
}

func (in *Installer) isActive(unit string) bool {
	out, err := in.Exec.Run("systemctl", "is-active", unit)
	return err == nil && strings.TrimSpace(out) == "active"
}

func (in *Installer) stepEnable() (bool, string, error) {
	var enabled []string
	for _, u := range in.unitNames() {
		if in.isEnabled(u) {
			continue
		}
		if _, err := in.Exec.Sudo("systemctl", "enable", u); err != nil {
			return len(enabled) > 0, "", err
		}
		enabled = append(enabled, u)
	}
	if len(enabled) == 0 {
		return false, "enabled", nil
	}
	return true, "enabled " + strings.Join(enabled, ", "), nil
}

// stepStart starts what is down and restarts what changed underneath a
// running unit. The socket is never restarted: it holds the loopback port
// and the service picks up the new binary on its own restart.
func (in *Installer) stepStart(restart bool) (bool, string, error) {
	if in.NoStart {
		return false, "skipped (--no-start)", nil
	}
	var done []string
	for _, u := range in.unitNames() {
		switch {
		case !in.isActive(u):
			if _, err := in.Exec.Sudo("systemctl", "start", u); err != nil {
				return len(done) > 0, "", err
			}
			done = append(done, "started "+u)
		case restart && u != SocketUnit:
			if _, err := in.Exec.Sudo("systemctl", "restart", u); err != nil {
				return len(done) > 0, "", err
			}
			done = append(done, "restarted "+u)
		}
	}
	if len(done) == 0 {
		return false, "running", nil
	}
	return true, strings.Join(done, ", "), nil
}

// stepSetup runs `middleware setup` once, so the install ends with the
// one-time code on screen instead of a second command to remember.
func (in *Installer) stepSetup() (bool, string, error) {
	exists := in.OperatorExists(in.Params.Root)
	if exists && in.Params.Domain == "" {
		return false, "exists", nil
	}
	if in.NoStart {
		return false, fmt.Sprintf("none yet: run `%s setup --root %s` after starting", in.Params.Binary, in.Params.Root), nil
	}
	args := []string{"setup", "--root", in.Params.Root}
	if in.Params.Domain != "" {
		args = append(args, "--domain", in.Params.Domain)
		if in.Params.Door != "" {
			args = append(args, "--door", in.Params.Door)
		}
	}
	out, err := in.Exec.Run(in.Params.Binary, args...)
	if err != nil {
		return false, fmt.Sprintf("none yet: run `%s setup --root %s` to create one (%v)", in.Params.Binary, in.Params.Root, err), nil
	}
	in.setupOutput = out
	if exists {
		return true, "the panel's domain is " + in.Params.Domain, nil
	}
	return true, "created; the one-time code is below", nil
}
