package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/user"
	"strings"

	"github.com/dcimorra/middleware-public/internal/installer"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/settings"
	"github.com/dcimorra/middleware-public/internal/shots"
)

func init() {
	subcommands["install"] = installCmd
	subcommands["uninstall"] = uninstallCmd
	subcommands["doctor"] = doctorCmd
	extraUsage += `  install      install units, group and root directory (uses sudo where needed)
  uninstall    stop and remove the units; keeps the root directory and the sites
  doctor       check the installation and print one line per check
`
}

// installFlags are shared by install, uninstall and doctor so the three
// agree on where things are.
func installFlags(fs *flag.FlagSet, p *installer.Params) {
	fs.StringVar(&p.Root, "root", "", "installation root (default $MIDDLEWARE_ROOT or /srv/middleware)")
	fs.StringVar(&p.User, "user", "", "user that runs the supervisor and owns the root (default: you)")
	fs.StringVar(&p.Binary, "binary", p.Binary, "where the executable is installed")
}

func installParams(fs *flag.FlagSet, args []string) (installer.Params, error) {
	p := installer.DefaultParams()
	p.Version = Version
	installFlags(fs, &p)
	noHelper := fs.Bool("no-helper", false, "do not install the privileged helper (firewall, vpn and sysctl stay unavailable)")
	fs.StringVar(&p.Domain, "domain", os.Getenv("MIDDLEWARE_DOMAIN"), "the panel's domain (asked when there is a terminal; MIDDLEWARE_DOMAIN works too)")
	fs.StringVar(&p.Browser, "browser", os.Getenv("MIDDLEWARE_BROWSER"), "yes installs a headless Chromium for the site pictures when there is none (asked when there is a terminal)")
	fs.StringVar(&p.Door, "door", os.Getenv("MIDDLEWARE_DOOR"), "how the panel's domain is reached: ports (80/443) or tunnel (asked when there is a terminal; MIDDLEWARE_DOOR works too)")
	if err := fs.Parse(args); err != nil {
		return p, err
	}
	if *noHelper {
		p.Helper = false
	}
	// --root "" means "resolve like serve does", except that a bare cwd
	// with a sites/ folder is not an installation target: an operator who
	// runs install from inside a checkout does not want /etc pointing at it.
	if p.Root == "" {
		if v := os.Getenv("MIDDLEWARE_ROOT"); v != "" {
			p.Root = v
		} else {
			p.Root = "/srv/middleware"
		}
	}
	if p.User == "" {
		u, err := user.Current()
		if err != nil {
			return p, fmt.Errorf("cannot tell who you are: %w (pass --user)", err)
		}
		p.User = u.Username
	}
	// The listen address and port range come from the installation's own
	// settings when it already has them, so the socket unit and the sysctl
	// line agree with what serve will use.
	if s, err := settings.Load(p.Root); err == nil {
		p.Listen = s.Listen
		p.PortsFrom, p.PortsTo = s.Ports.From, s.Ports.To
	}
	return p, nil
}

func installCmd(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	noStart := fs.Bool("no-start", false, "render and enable the units but do not start them")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	panel := fs.Bool("panel", false, "started from the panel (the helper's requirements.install): the last line says how it ended, for the panel to read")
	deps := fs.Bool("requirements", false, "only the house's requirements: packages, docker, nvm, fail2ban and, with --browser yes, a headless Chromium; no unit, binary or restart")
	p, err := installParams(fs, args)
	if err != nil {
		return err
	}
	if *deps {
		in := installer.New(p)
		in.Live = isTerminal(os.Stdout)
		_, restart, err := in.Requirements()
		if *panel {
			// What the panel reads to know it ended: the unit is collected
			// when it exits, and its status with it.
			switch {
			case err != nil:
				fmt.Printf("\n%s%v\n", installer.PanelFailed, err)
			case restart:
				fmt.Printf("\n%s\n", installer.PanelRestart)
			default:
				fmt.Printf("\n%s\n", installer.PanelFinished)
			}
		}
		return err
	}
	return runInstall(p, *noStart, *yes)
}

func runInstall(p installer.Params, noStart, yes bool) error {
	if p.User == "root" {
		return errors.New("refusing to install for root: run as the user that will operate the panel; sudo is used where needed")
	}
	// The panel's name, asked here because install.sh is where a new
	// server starts (it hands us the terminal even when piped from curl).
	// Without one the panel is loopback only and the first sign-in needs
	// an SSH tunnel, which is the step a new operator does not know.
	if p.Domain == "" && isTerminal(os.Stdin) {
		if s, err := settings.Load(p.Root); err != nil || s.PanelDomain == "" {
			fmt.Print("Domain for the panel, e.g. panel.example.com (point its DNS at this server).\nEmpty keeps it on this machine only, reached through an SSH tunnel: ")
			line, _ := stdin().ReadString('\n')
			p.Domain = strings.ToLower(strings.TrimSpace(line))
			// Neither door is required: a server behind a NAT may have
			// no 80/443, and one without Cloudflare no tunnel.
			if p.Domain != "" && p.Door == "" {
				fmt.Print("How is it reached? 1) ports 80/443, with a certificate of its own  2) a Cloudflare tunnel [1]: ")
				line, _ := stdin().ReadString('\n')
				if a := strings.TrimSpace(strings.ToLower(line)); a == "2" || a == "tunnel" {
					p.Door = "tunnel"
				}
			}
		}
	}
	if p.Door == "" {
		p.Door = "ports"
	}
	// Asked only when there is none: an installed browser (the system's,
	// Playwright's, an earlier install) is used as it is.
	if p.Browser == "" && isTerminal(os.Stdin) && shots.FindIn(p.Root) == "" {
		fmt.Printf("%s\nInstall Chrome for Testing's headless shell (over 100 MB, and the system libraries it loads)? [Y/n] ", installer.BrowserWhy)
		line, _ := stdin().ReadString('\n')
		if a := strings.TrimSpace(strings.ToLower(line)); a == "" || a == "y" || a == "yes" {
			p.Browser = "yes"
		} else {
			p.Browser = "no"
		}
	}
	if p.Door != "ports" && p.Door != "tunnel" {
		return fmt.Errorf("door %q: want ports or tunnel", p.Door)
	}
	if p.Domain != "" && !manifest.ValidDomain(strings.TrimSuffix(p.Domain, ".")) {
		return fmt.Errorf("%q is not a domain name", p.Domain)
	}
	in := installer.New(p)
	in.NoStart = noStart
	in.Live = isTerminal(os.Stdout)
	panel := p.Domain + " (" + p.Door + ")"
	if p.Domain == "" {
		panel = "none (loopback only)"
		// A re-run keeps the name an earlier one gave it.
		if s, err := settings.Load(p.Root); err == nil && s.PanelDomain != "" {
			panel = s.PanelDomain + " (kept)"
		}
	}
	browser := shots.FindIn(p.Root)
	switch {
	case browser != "":
	case p.Browser == "yes":
		browser = "to install"
	default:
		browser = "none (no site pictures)"
	}
	fmt.Printf("middleware %s install\n  user    %s\n  group   %s\n  root    %s\n  binary  %s\n  helper  %v\n  panel   %s\n  browser %s\n\n", Version, p.User, p.Group, p.Root, p.Binary, p.Helper, panel, browser)
	if !yes && !confirm("Continue? Steps that need root will ask for your sudo password. [y/N] ") {
		return errors.New("cancelled")
	}
	if _, err := in.Install(); err != nil {
		return err
	}
	fmt.Println("\nDone. `middleware doctor` checks the installation at any time.")
	return nil
}

func uninstallCmd(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	keep := fs.Bool("keep-binary", false, "leave the executable in place")
	p, err := installParams(fs, args)
	if err != nil {
		return err
	}
	in := installer.New(p)
	in.KeepBinary = *keep
	fmt.Printf("This stops middleware and removes its units. The root directory %s and every site in it stay where they are.\n", p.Root)
	if !*yes && !confirm("Continue? [y/N] ") {
		return errors.New("cancelled")
	}
	_, err = in.Uninstall()
	return err
}

func doctorCmd(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	p, err := installParams(fs, args)
	if err != nil {
		return err
	}
	in := installer.New(p)
	if _, ok := in.Doctor(); !ok {
		return errors.New("some checks failed")
	}
	return nil
}

// stdin is one reader for the whole run: two prompts each with a reader
// of its own would lose the second answer in the first one's buffer.
var stdinReader *bufio.Reader

func stdin() *bufio.Reader {
	if stdinReader == nil {
		stdinReader = bufio.NewReader(os.Stdin)
	}
	return stdinReader
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func confirm(prompt string) bool {
	fmt.Print(prompt)
	line, _ := stdin().ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}
