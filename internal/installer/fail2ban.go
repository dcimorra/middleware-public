package installer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// fail2ban, for what the house does not see. The web already has its own
// bans inside the WAF (docs/security.md, "Bans"), and a second set of rules
// over the same requests would ban by other rules and lift by other
// timers. What the house never sees is SSH: that is fail2ban's jail here,
// with the house's own numbers (five failures in ten minutes, an hour,
// four times longer at every repeat, thirty days at most) and the house's
// own exceptions (loopback, the private and CGNAT ranges the VPN and the
// mesh live in).
//
// Integrated rather than beside it: every ban fail2ban makes is also
// written into the house's block list through `middleware waf block`, so
// it shows in the panel with the others, and is pushed to the kernel by the
// helper for every port, not only SSH's; fail2ban's own nftables ban stays
// as well, so SSH is closed to that address even with the house down.
// fail2ban lifting it lifts it in the house; the panel lifting it does not
// reach fail2ban, whose SSH ban runs its time.

// Fail2banJail and Fail2banAction are the files under Paths.Fail2banDir.
const (
	Fail2banJail   = "jail.d/middleware.conf"
	Fail2banAction = "action.d/middleware.conf"
)

// fail2banIgnore are the addresses never banned: the house's own list.
const fail2banIgnore = "127.0.0.1/8 ::1 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 fc00::/7"

// RenderFail2ban is the jail and the action, by their path under the
// fail2ban folder.
func RenderFail2ban(p Params) map[string]string {
	jail := `# Written by middleware install; it rewrites this file when it differs.
# SSH, with the house's numbers; every ban is also the house's (action
# "middleware"), shown in its panel and closed in the kernel for every port.
[DEFAULT]
ignoreip = ` + fail2banIgnore + `

[sshd]
enabled = true
backend = systemd
banaction = nftables
action = %(action_)s
         middleware
maxretry = 5
findtime = 10m
bantime = 1h
bantime.increment = true
bantime.factor = 4
bantime.formula = ban.Time * (1<<(ban.Count if ban.Count<20 else 20)) * banFactor
bantime.maxtime = 30d
`
	run := "runuser -u " + p.User + " -- " + p.Binary
	action := `# Written by middleware install; it rewrites this file when it differs.
# A ban fail2ban makes is written into the house's block list, as the
# operator's user (the files under the root are theirs), and lifted there
# when fail2ban lifts it.
[Definition]
actionstart =
actionstop =
actioncheck =
actionban = ` + run + ` waf block --root ` + p.Root + ` <ip> <bantime>s fail2ban <name>
actionunban = ` + run + ` waf unblock --root ` + p.Root + ` <ip>
`
	return map[string]string{Fail2banJail: jail, Fail2banAction: action}
}

// fail2banPackages is the package in each distribution: the same name.
var fail2banPackages = map[string]string{"apt-get": "fail2ban", "dnf": "fail2ban", "apk": "fail2ban", "pacman": "fail2ban", "zypper": "fail2ban"}

func (in *Installer) fail2banDir() string {
	if in.Paths.Fail2banDir != "" {
		return in.Paths.Fail2banDir
	}
	return "/etc/fail2ban"
}

// stepFail2ban installs fail2ban when missing, writes the jail and the
// action, and has the service running them.
func (in *Installer) stepFail2ban() (bool, string, error) {
	var did []string
	installed := false
	if _, err := in.Exec.LookPath("fail2ban-client"); err != nil {
		cmd, name := in.packageManager()
		pkg := fail2banPackages[name]
		if cmd == nil || pkg == "" {
			return false, "", fmt.Errorf("fail2ban is not installed and there is no package manager here to install it with")
		}
		args := append(append([]string{}, cmd[1:]...), pkg)
		if _, err := in.Exec.Sudo(cmd[0], args...); err != nil {
			return false, "", err
		}
		installed = true
		did = append(did, "installed with "+name)
	}
	files := RenderFail2ban(in.Params)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var written []string
	for _, n := range names {
		p := filepath.Join(in.fail2banDir(), n)
		if cur, err := os.ReadFile(p); err == nil && string(cur) == files[n] {
			continue
		}
		if err := in.Exec.WriteRoot(p, []byte(files[n]), 0o644); err != nil {
			return installed || len(written) > 0, "", err
		}
		written = append(written, n)
	}
	if len(written) > 0 {
		did = append(did, "wrote "+strings.Join(written, ", "))
	}
	if installed || !in.isActive("fail2ban") {
		if _, err := in.Exec.Sudo("systemctl", "enable", "--now", "fail2ban"); err != nil {
			return true, "", fmt.Errorf("fail2ban could not be started: %w", err)
		}
		did = append(did, "service enabled and started")
	} else if len(written) > 0 {
		if _, err := in.Exec.Sudo("fail2ban-client", "reload"); err != nil {
			return true, "", fmt.Errorf("fail2ban did not take the new jail: %w", err)
		}
		did = append(did, "reloaded")
	}
	if len(did) == 0 {
		return false, "sshd jail, bans shared with the house", nil
	}
	return true, strings.Join(did, "; "), nil
}

// removeFail2ban takes the house's jail and action out on uninstall; the
// package stays, it is the machine's.
func (in *Installer) removeFail2ban() {
	removed := false
	for _, n := range []string{Fail2banJail, Fail2banAction} {
		p := filepath.Join(in.fail2banDir(), n)
		if _, err := os.Stat(p); err == nil {
			_ = in.Exec.RemoveRoot(p)
			removed = true
		}
	}
	if removed && in.isActive("fail2ban") {
		_, _ = in.Exec.Sudo("fail2ban-client", "reload")
	}
}
