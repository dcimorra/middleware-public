package installer

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dcimorra/middleware-public/internal/fsx"
)

// Check is one line of `middleware doctor`.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// String is the line as printed: a mark, the name in a fixed column, the
// detail. Fixed width so the eye can scan the marks.
func (c Check) String() string {
	mark := "✓"
	if !c.OK {
		mark = "✗"
	}
	return fmt.Sprintf("%s %-26s %s", mark, c.Name, c.Detail)
}

// Doctor runs every check and writes one line each. It returns the checks
// and whether all of them passed; it never stops early, because the point
// is to see everything that is wrong at once.
func (in *Installer) Doctor() ([]Check, bool) {
	if in.Exec == nil {
		in.Exec = System{}
	}
	if in.Out == nil {
		in.Out = io.Discard
	}
	if in.Paths == (Paths{}) {
		in.Paths = SystemPaths()
	}
	checks := []Check{
		in.checkBinary(),
	}
	checks = append(checks, in.checkUnits()...)
	checks = append(checks,
		in.checkSocket(),
		in.checkHelper(),
		in.checkRootDir(),
		in.checkPorts(),
		in.checkDocker(),
		in.checkWireGuard(),
		in.checkDisk(),
	)
	all := true
	for _, c := range checks {
		fmt.Fprintln(in.Out, c.String())
		all = all && c.OK
	}
	return checks, all
}

func (in *Installer) checkBinary() Check {
	if !fsx.Exists(in.Params.Binary) {
		return Check{"binary", false, in.Params.Binary + " is missing: run `middleware install`"}
	}
	out, err := in.Exec.Run(in.Params.Binary, "version")
	if err != nil {
		return Check{"binary", false, fmt.Sprintf("%s does not run: %v", in.Params.Binary, err)}
	}
	v := strings.TrimPrefix(strings.TrimSpace(out), "middleware ")
	if in.Params.Version != "" && in.Params.Version != "dev" && v != in.Params.Version {
		return Check{"binary", false, fmt.Sprintf("%s is %s, this command is %s", in.Params.Binary, v, in.Params.Version)}
	}
	return Check{"binary", true, fmt.Sprintf("%s %s", in.Params.Binary, v)}
}

func (in *Installer) checkUnits() []Check {
	var out []Check
	for _, u := range in.unitNames() {
		if !fsx.Exists(filepath.Join(in.Paths.SystemdDir, u)) {
			out = append(out, Check{u, false, "unit file missing: run `middleware install`"})
			continue
		}
		state, _ := in.Exec.Run("systemctl", "is-active", u)
		state = strings.TrimSpace(state)
		if state == "" {
			state = "unknown"
		}
		out = append(out, Check{u, state == "active", state})
	}
	return out
}

func (in *Installer) checkSocket() Check {
	c, err := net.DialTimeout("tcp", in.Params.Listen, 2*time.Second)
	if err != nil {
		return Check{"loopback socket", false, fmt.Sprintf("%s not accepting: %v", in.Params.Listen, err)}
	}
	c.Close()
	return Check{"loopback socket", true, in.Params.Listen + " accepts connections"}
}

func (in *Installer) checkHelper() Check {
	if !in.Params.Helper {
		return Check{"helper", true, "not installed (--no-helper); firewall, vpn and sysctl operations are unavailable"}
	}
	c, err := net.DialTimeout("unix", in.Params.HelperSocket, 2*time.Second)
	if err != nil {
		hint := ""
		if os.IsPermission(err) || strings.Contains(err.Error(), "permission denied") {
			hint = fmt.Sprintf(" (is %s in the %s group, and has it logged in again?)", in.Params.User, in.Params.Group)
		}
		return Check{"helper", false, fmt.Sprintf("%s: %v%s", in.Params.HelperSocket, err, hint)}
	}
	c.Close()
	return Check{"helper", true, in.Params.HelperSocket + " reachable"}
}

func (in *Installer) checkRootDir() Check {
	st, err := os.Stat(in.Params.Root)
	if err != nil {
		return Check{"root directory", false, fmt.Sprintf("%s: %v", in.Params.Root, err)}
	}
	if !st.IsDir() {
		return Check{"root directory", false, in.Params.Root + " is not a directory"}
	}
	var missing []string
	for _, d := range rootSubdirs {
		if !fsx.Exists(filepath.Join(in.Params.Root, d)) {
			missing = append(missing, d+"/")
		}
	}
	if len(missing) > 0 {
		return Check{"root directory", false, fmt.Sprintf("%s lacks %s", in.Params.Root, strings.Join(missing, ", "))}
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && in.Params.User != "" {
		if u, err := user.Lookup(in.Params.User); err == nil {
			if uid, _ := strconv.Atoi(u.Uid); uint32(uid) != sys.Uid {
				owner := strconv.Itoa(int(sys.Uid))
				if o, err := user.LookupId(owner); err == nil {
					owner = o.Username
				}
				return Check{"root directory", false, fmt.Sprintf("%s is owned by %s, not %s", in.Params.Root, owner, in.Params.User)}
			}
		}
	}
	return Check{"root directory", true, fmt.Sprintf("%s (%s)", in.Params.Root, st.Mode().Perm())}
}

func (in *Installer) checkPorts() Check {
	want := fmt.Sprintf("%d-%d", in.Params.PortsFrom, in.Params.PortsTo)
	out, err := in.Exec.Run("sysctl", "-n", "net.ipv4.ip_local_reserved_ports")
	if err != nil {
		return Check{"reserved ports", false, fmt.Sprintf("cannot read sysctl: %v", err)}
	}
	for _, r := range strings.Split(strings.TrimSpace(out), ",") {
		if r == want {
			return Check{"reserved ports", true, want + " kept out of the ephemeral range"}
		}
	}
	return Check{"reserved ports", false, fmt.Sprintf("%s not reserved (have %q): run `middleware install`", want, strings.TrimSpace(out))}
}

func (in *Installer) checkDocker() Check {
	if _, err := in.Exec.LookPath("docker"); err != nil {
		return Check{"docker", true, "not installed (the docker runtime is optional)"}
	}
	out, err := in.Exec.Run("docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return Check{"docker", false, fmt.Sprintf("installed but the daemon does not answer: %v", err)}
	}
	return Check{"docker", true, "daemon " + strings.TrimSpace(out)}
}

func (in *Installer) checkWireGuard() Check {
	if _, err := in.Exec.LookPath("wg"); err != nil {
		return Check{"wireguard", true, "wg not installed (the vpn module installs it when enabled)"}
	}
	return Check{"wireguard", true, "wg present"}
}

// lowDisk is where the line turns red: a build of any modern site needs
// more than this, and a full disk takes the journal and the manifests with
// it.
const lowDisk = 1 << 30

func (in *Installer) checkDisk() Check {
	var st syscall.Statfs_t
	if err := syscall.Statfs(in.Params.Root, &st); err != nil {
		return Check{"disk", false, fmt.Sprintf("%s: %v", in.Params.Root, err)}
	}
	free := uint64(st.Bavail) * uint64(st.Bsize)
	detail := fmt.Sprintf("%s free on %s", humanBytes(free), in.Params.Root)
	if free < lowDisk {
		return Check{"disk", false, detail + " (less than 1 GiB)"}
	}
	return Check{"disk", true, detail}
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
