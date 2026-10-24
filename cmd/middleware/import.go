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

	"github.com/dcimorra/middleware-public/internal/relocate"
)

func init() {
	subcommands["import"] = importCmd
	extraUsage += `  import --to <url> --token <token> <V1 root>/sites/<domain>
               run on a V1 machine: move one site into a V2 house (the token is made in its panel)
`
}

// importCmd takes a V1 site into a V2 house. It runs on the V1 machine,
// with the V2 binary and a token the V2 panel made for this one site: the
// site is copied while it serves, stopped in V1 for the last copy, put in
// place and started in V2, and left stopped in V1 (started again there if
// the V2 copy does not come up). The names stay on V1 until the operator
// points them at V2 from the V2 panel.
func importCmd(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	to := fs.String("to", "", "the V2 house's address (https://panel.example.com)")
	token := fs.String("token", "", "the import token its panel made for this site")
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 || *to == "" || *token == "" {
		return errors.New("usage: middleware import --to <url> --token <token> <V1 root>/sites/<domain>")
	}
	dir, err := filepath.Abs(strings.TrimRight(rest[0], "/"))
	if err != nil {
		return err
	}
	domain := filepath.Base(dir)
	if filepath.Base(filepath.Dir(dir)) != "sites" {
		return fmt.Errorf("%s is not a folder inside a house's sites/", dir)
	}
	root := filepath.Dir(filepath.Dir(dir))
	src := &relocate.V1Source{Root: root}
	notes, err := src.Convert(domain)
	if err != nil {
		return fmt.Errorf("%s: %w", domain, err)
	}
	for _, n := range notes {
		fmt.Printf("  note: %s\n", n)
	}
	target := relocate.HTTPTarget{Base: *to, Token: *token, Install: true}
	mover := &relocate.Mover{Root: root, Source: src, Parts: relocate.ImportParts, ReadyWait: 10 * time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	start := time.Now()
	rep, err := mover.Move(ctx, domain, target, func(step, msg string) {
		fmt.Printf("%s  %-8s %s\n", time.Since(start).Round(time.Second), step, msg)
	})
	if err != nil {
		return err
	}
	fmt.Printf("\n%s runs on %s: it was down %s (%d bytes first, %d the last copy).\n", domain, *to, rep.Down, rep.First, rep.Last)
	fmt.Printf("It is stopped here, its folder where it was. In the V2 panel: Publish it, then \"Point it here\" for each name V1 still has.\n")
	fmt.Printf("Once it has served a while there, archive it in the V1 panel. To go back now: remove \"disabled\" from %s.\n", filepath.Join(dir, "site.json"))
	_ = os.Stdout.Sync()
	return nil
}
