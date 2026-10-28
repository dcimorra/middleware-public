package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dcimorra/middleware-public/internal/chat"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/runtime"
	"github.com/dcimorra/middleware-public/internal/settings"
)

func init() {
	subcommands["exec"] = execCommand
	extraUsage += `  exec <domain> [--] <command…>
               run a command where the site runs (inside its container if it has one)
`
}

// execCommand needs no running supervisor: it reads the manifest and runs
// the command itself, so it works while the site is down, which is when an
// install or a migration is usually needed.
func execCommand(args []string) error {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	root := fs.String("root", "", "installation root (default $MIDDLEWARE_ROOT or /srv/middleware)")
	tty := fs.Bool("t", false, "allocate a terminal (an interactive shell)")
	// Flags only before the domain: everything after it belongs to the
	// command, which has flags of its own (npm install --omit=dev).
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return fmt.Errorf("usage: middleware exec <domain> [--] <command…>")
	}
	domain := strings.ToLower(rest[0])
	cmdArgs := rest[1:]
	if cmdArgs[0] == "--" {
		cmdArgs = cmdArgs[1:]
	}
	if !manifest.ValidName(domain) {
		return fmt.Errorf("%q is not a site or project", domain)
	}
	// The skill tells a docker site's conversation to install with
	// `middleware exec <its domain>`, and the command needs no supervisor:
	// from a site's turn, `middleware exec other.site -- env` printed the
	// other site's secrets, the very folder its jail denies. A turn may
	// exec in its own site and nowhere else; one that cannot be told
	// (two sites' marks in its chain) nowhere.
	turn, err := chat.SiteOfProcess("/proc", os.Getpid(), nil)
	if err != nil {
		return fmt.Errorf("exec refused: %w", err)
	}
	if turn != "" && turn != domain {
		return fmt.Errorf("exec refused: this runs under a turn of %s's conversation, which may exec in %s only", turn, turn)
	}
	dir := settings.Root(*root)
	siteDir := filepath.Join(dir, "sites", domain)
	m, err := manifest.Load(siteDir)
	if err != nil {
		return err
	}
	if m.Run == nil {
		// A static site or a project still gets its folder and HOME.
		m = &manifest.Manifest{Version: manifest.Version, Run: &manifest.Run{Command: []string{"true"}}}
	}
	spec, err := manifest.Resolve(domain, siteDir, m)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code, err := runtime.Exec(ctx, spec, filepath.Join(dir, "data", "homes", domain), runtime.ExecOptions{
		Args: cmdArgs, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, TTY: *tty,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}
