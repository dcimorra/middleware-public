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

	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/relocate"
)

func init() {
	subcommands["import"] = importCmd
	extraUsage += `  import --to <url> --token <token> [--domain d] [--command "…"] [--stop "…"] [--start "…"] <folder>
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
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 || *to == "" || *token == "" {
		return errors.New("usage: middleware import --to <url> --token <token> [--domain d] [--command …] [--stop …] [--start …] <folder>")
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
	mover := &relocate.Mover{ReadyWait: 10 * time.Minute}
	v1 := filepath.Base(filepath.Dir(dir)) == "sites" && filepath.Base(dir) == domain && *command == "" && *stop == ""
	if v1 {
		root := filepath.Dir(filepath.Dir(dir))
		src := &relocate.V1Source{Root: root}
		if notes, err := src.Convert(domain); err == nil {
			for _, n := range notes {
				fmt.Printf("  note: %s\n", n)
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	begin := time.Now()
	rep, err := mover.Move(ctx, domain, target, func(step, msg string) {
		fmt.Printf("%s  %-8s %s\n", time.Since(begin).Round(time.Second), step, msg)
	})
	if err != nil {
		return err
	}
	fmt.Printf("\n%s runs on %s: it was down %s (%d bytes first, %d the last copy).\n", domain, *to, rep.Down, rep.First, rep.Last)
	fmt.Printf("In the V2 panel: Publish it, then \"Point it here\" for each name that still points at this machine.\n")
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
