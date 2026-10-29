package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/relocate"
)

func init() {
	subcommands["import"] = importCmd
	extraUsage += `  import --to <url> --token <token> [--domain d] [--command "…"] [--stop "…"] [--start "…"] [--tasks-off] <folder>
               move one site or app into a V2 house (the token is made in its panel): a V1 house's
               site (<root>/sites/<domain>) or any app folder on any server
`
}

// importCmd takes a site into a V2 house, run on the machine where it is
// now, with the V2 binary and a token the V2 panel made for this one site.
// A V1 house's site is stopped and started the way V1 does it; any other
// app with the commands given (--stop, --start), or copied running when
// none is. The names stay where they are until the operator points them
// at V2 from the V2 panel.
func importCmd(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	to := fs.String("to", "", "the V2 house's address (https://panel.example.com)")
	token := fs.String("token", "", "the import token its panel made for this site")
	domainFlag := fs.String("domain", "", "the site's domain (default: the folder's name)")
	command := fs.String("command", "", "how the app starts, when its folder does not say (\"node server.js\")")
	stop := fs.String("stop", "", "a shell line that stops the app here for the last copy (\"pm2 stop shop\")")
	start := fs.String("start", "", "a shell line that starts it here again if the import fails")
	tasksOff := fs.Bool("tasks-off", false, "the site's tasks arrive turned off (and are turned off here, as always)")
	dryRun := fs.Bool("dry-run", false, "say what the import would do (what is copied, where dependencies are installed, what will not travel well) and stop")
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 || *to == "" || *token == "" {
		return errors.New("usage: middleware import --to <url> --token <token> [--domain d] [--command …] [--stop …] [--start …] [--tasks-off] <folder>")
	}
	dir, err := filepath.Abs(strings.TrimRight(rest[0], "/"))
	if err != nil {
		return err
	}
	domain := strings.ToLower(strings.TrimSpace(*domainFlag))
	if domain == "" {
		domain = filepath.Base(dir)
	}
	if !manifest.ValidName(domain) {
		return fmt.Errorf("%q is not a domain: say it with --domain", domain)
	}
	target := relocate.HTTPTarget{Base: *to, Token: *token, Install: true}
	mover := &relocate.Mover{ReadyWait: 10 * time.Minute, Install: true}
	v1 := filepath.Base(filepath.Dir(dir)) == "sites" && filepath.Base(dir) == domain && *command == "" && *stop == ""
	if v1 {
		root := filepath.Dir(filepath.Dir(dir))
		src := &relocate.V1Source{Root: root, TasksOff: *tasksOff}
		if notes, err := src.Convert(domain); err == nil {
			for _, n := range notes {
				fmt.Printf("  note: %s\n", n)
			}
			// Before anything moves: a task that does not convert stops
			// the import here, not after the site is down.
			taskNotes, err := src.TaskNotes(domain)
			if err != nil {
				return fmt.Errorf("its tasks: %w", err)
			}
			for _, n := range taskNotes {
				fmt.Printf("  plan: %s\n", n)
			}
			mover.Root, mover.Source, mover.Parts = root, src, relocate.ImportParts
		} else {
			v1 = false
		}
	}
	if !v1 {
		var cmd []string
		if *command != "" {
			cmd = strings.Fields(*command)
		}
		src := &relocate.GenericSource{Dir: dir, Command: cmd, StopCmd: *stop, StartCmd: *start}
		how, err := src.Detect(domain)
		if err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
		fmt.Printf("  manifest: from %s\n", how)
		if *stop == "" {
			fmt.Printf("  note: no --stop given: it is copied while it runs; a database it writes meanwhile may arrive torn\n")
		}
		mover.Root, mover.Dir, mover.Source, mover.Parts = filepath.Dir(dir), dir, src, src.Parts
	}
	// What it will do, before it does it: the same analysis a move between
	// nodes shows in the panel.
	plan := relocate.Analyze(dir, mover.PartsFor(domain), true, nil)
	fmt.Printf("  plan: copies %.1f MB in %d files", float64(plan.Bytes)/(1<<20), plan.Files)
	if plan.Node != "" {
		fmt.Printf(", Node %s", plan.Node)
	}
	fmt.Println()
	if len(plan.Installs) == 0 {
		fmt.Println("  plan: no dependencies to install there")
	}
	for _, in := range plan.Installs {
		fmt.Printf("  plan: installs dependencies there in %s (%s)\n", in.Dir, strings.Join(in.Command[:2], " "))
	}
	for _, l := range plan.Links {
		fmt.Printf("  plan: link out of the folder: %s -> %s\n", l.Path, l.Target)
	}
	for _, w := range plan.Warnings {
		fmt.Printf("  warning: %s\n", w)
	}
	if *dryRun {
		// The target's answer too: what it would refuse is better known
		// now than with the site stopped.
		cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
		_, err := mover.Check(cctx, domain, target)
		ccancel()
		if err != nil {
			fmt.Printf("  check: %s would refuse it: %v\n", *to, err)
		} else {
			fmt.Printf("  check: %s can take it (its name, its names in DNS, its tasks, docker)\n", *to)
		}
		fmt.Println("Nothing was moved (--dry-run).")
		return err
	}
	// A backstop, not a budget: each step bounds itself (the install, each
	// move command by its timeout, the wait for the site), and a database
	// of tens of gigabytes made two hours too short for the whole.
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()
	begin := time.Now()
	rep, err := mover.Move(ctx, domain, target, func(step, msg string) {
		fmt.Printf("%s  %-8s %s\n", time.Since(begin).Round(time.Second), step, msg)
	})
	if err != nil {
		return err
	}
	fmt.Printf("\n%s runs on %s: it was down %s (%d bytes first, %d the last copy).\n", domain, *to, rep.Down, rep.First, rep.Last)
	switch {
	case rep.DNS == "moved":
		fmt.Printf("Its names point at %s now.\n", *to)
	case rep.DNS != "":
		fmt.Printf("Its names were not pointed there (%s): in the V2 panel, Publish it, then \"Point it here\" for each name still pointing at this machine.\n", rep.DNS)
	}
	if v1 {
		fmt.Printf("It is stopped here, its folder where it was. Once it has served a while there, archive it in the V1 panel; to go back now, remove \"disabled\" from %s.\n", filepath.Join(dir, "site.json"))
	} else if *stop != "" {
		fmt.Printf("It is stopped here (%s), its folder where it was; %s starts it again.\n", *stop, startOr(*start))
	}
	_ = os.Stdout.Sync()
	return nil
}

func startOr(s string) string {
	if s == "" {
		return "your own start command"
	}
	return s
}
