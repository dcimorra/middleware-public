package installer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/jail"
)

// A site's jail (internal/jail) is built by the supervisor itself, as its
// own unprivileged user, out of user namespaces. Two things of the
// machine can stand in its way, and this step clears both:
//
//   - Ubuntu 24.04 and later let a program make a user namespace only
//     when an AppArmor profile says it may (the sysctl
//     kernel.apparmor_restrict_unprivileged_userns). The profile below
//     says so for this binary and nothing else: it stays unconfined
//     otherwise, as it was.
//   - systemd's ProtectKernelTunables, which units rendered before the
//     jail had, over-mounts parts of /proc, and the kernel then refuses a
//     fresh /proc to a user namespace: the jail cannot have one. Those
//     units get a drop-in that turns it off. An update replaces the binary
//     and never the units, so the drop-in is how an installed machine
//     gets there without a reinstall.

// AppArmorProfile is the profile's file name under /etc/apparmor.d.
const AppArmorProfile = "middleware"

// JailDropIn is the service's drop-in that lets the jail mount /proc.
const JailDropIn = "middleware.service.d/jail.conf"

func (in *Installer) apparmorDir() string {
	if in.Paths.AppArmorDir != "" {
		return in.Paths.AppArmorDir
	}
	return "/etc/apparmor.d"
}

// RenderAppArmor is the profile for the binary at bin.
func RenderAppArmor(bin, version string) string {
	return fmt.Sprintf(`# %s, rendered by middleware %s install.
# Lets the supervisor build its sites' jails out of user namespaces, which
# this machine allows only to programs a profile names. Nothing else is
# confined or allowed: the profile is unconfined but for that.
abi <abi/4.0>,
include <tunables/global>

profile middleware %s flags=(unconfined) {
  userns,

  include if exists <local/middleware>
}
`, AppArmorProfile, version, bin)
}

const jailDropInContent = `# Rendered by middleware install. The sites' jails mount a /proc of their
# own, which the kernel refuses while parts of the machine's are
# over-mounted, as ProtectKernelTunables does.
[Service]
ProtectKernelTunables=no
`

// usernsRestricted says whether this machine wants a profile for user
// namespaces.
func (in *Installer) usernsRestricted() bool {
	out, err := in.Exec.Run("sysctl", "-n", "kernel.apparmor_restrict_unprivileged_userns")
	return err == nil && strings.TrimSpace(out) == "1"
}

// stepJail writes what the jail needs. It says it changed something when
// the supervisor must restart to see it.
func (in *Installer) stepJail() (bool, string, error) {
	var did []string
	changed := false
	if in.usernsRestricted() {
		p := filepath.Join(in.apparmorDir(), AppArmorProfile)
		want := RenderAppArmor(in.Params.Binary, in.Params.Version)
		if cur, err := os.ReadFile(p); err != nil || string(cur) != want {
			if err := in.Exec.WriteRoot(p, []byte(want), 0o644); err != nil {
				return false, "", err
			}
			if _, err := in.Exec.Sudo("apparmor_parser", "-r", p); err != nil {
				return true, "", fmt.Errorf("loading the AppArmor profile: %w", err)
			}
			did = append(did, "AppArmor profile loaded")
			changed = true
		}
	}
	if unit, err := os.ReadFile(in.pathFor(ServiceUnit)); err == nil && strings.Contains(string(unit), "\nProtectKernelTunables=yes") {
		p := filepath.Join(in.Paths.SystemdDir, JailDropIn)
		if cur, err := os.ReadFile(p); err != nil || string(cur) != jailDropInContent {
			if err := in.Exec.WriteRoot(p, []byte(jailDropInContent), 0o644); err != nil {
				return changed, "", err
			}
			if _, err := in.Exec.Sudo("systemctl", "daemon-reload"); err != nil {
				return true, "", err
			}
			did = append(did, "service may mount a jail's /proc")
			changed = true
		}
	}
	if len(did) == 0 {
		return false, "ready", nil
	}
	return changed, strings.Join(did, ", "), nil
}

// jailFiles are the files stepJail may have written, for uninstall.
func (in *Installer) jailFiles() []string {
	return []string{filepath.Join(in.apparmorDir(), AppArmorProfile), filepath.Join(in.Paths.SystemdDir, JailDropIn)}
}

// checkJail says whether the sites run in jails of their own, or what
// stands in the way. The supervisor says the same on the panel when it
// starts (jail.off), from inside its unit, which is the final word: this
// runs in the operator's shell.
func (in *Installer) checkJail() Check {
	if in.usernsRestricted() && !fsx.Exists(filepath.Join(in.apparmorDir(), AppArmorProfile)) {
		return Check{"site jails", false, "this machine allows user namespaces only to programs AppArmor names, and middleware has no profile yet: run `middleware install` again (or Install requirements on the panel)"}
	}
	if unit, err := os.ReadFile(in.pathFor(ServiceUnit)); err == nil && strings.Contains(string(unit), "\nProtectKernelTunables=yes") && !fsx.Exists(filepath.Join(in.Paths.SystemdDir, JailDropIn)) {
		return Check{"site jails", false, "the service unit keeps ProtectKernelTunables on, which leaves the jails without a /proc: run `middleware install` again (or Install requirements on the panel)"}
	}
	if err := jail.Available(); err != nil {
		return Check{"site jails", false, err.Error()}
	}
	return Check{"site jails", true, "each site sees only its own folder"}
}
