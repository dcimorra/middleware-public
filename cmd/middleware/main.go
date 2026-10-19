// Command middleware is the whole product: the supervisor (`serve`), the
// privileged helper (`helper`), the installer and the operator CLI, in one
// binary so there is exactly one thing to build and ship.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/dcimorra/middleware-public/internal/app"
	"github.com/dcimorra/middleware-public/internal/settings"
)

// Version is set by the build (-ldflags "-X main.Version=...").
var Version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "version":
		fmt.Println("middleware", Version)
	case "help", "-h", "--help":
		usage()
	default:
		err = dispatch(cmd, args)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "middleware:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: middleware <command> [flags]

  serve        run the supervisor (what the systemd unit runs)
  version      print the version
`+extraUsage)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	root := fs.String("root", "", "installation root (default $MIDDLEWARE_ROOT or /srv/middleware)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	a, err := app.New(settings.Root(*root))
	if err != nil {
		return err
	}
	a.Hooks = hooks(a)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return a.Run(ctx)
}
