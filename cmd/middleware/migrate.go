package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dcimorra/the-middleware/internal/migrate"
	"github.com/dcimorra/the-middleware/internal/settings"
)

func init() {
	subcommands["migrate"] = migrateCommand
	extraUsage += `  migrate      convert V1 manifests under sites/ (--apply to write; --sites <dir>)
`
}

// migrateCommand reports by default and writes only with --apply: a
// conversion that runs on the first keystroke is not one anyone checked.
func migrateCommand(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	root := fs.String("root", "", "installation root (default $MIDDLEWARE_ROOT or /srv/middleware)")
	sitesDir := fs.String("sites", "", "folder of sites (default <root>/sites)")
	apply := fs.Bool("apply", false, "write the converted manifests, keeping site.v1.json")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if _, err := parseInterleaved(fs, args); err != nil {
		return err
	}
	dir := *sitesDir
	if dir == "" {
		dir = filepath.Join(settings.Root(*root), "sites")
	}
	results, err := migrate.Plan(dir)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return err
		}
	} else {
		printPlan(results)
	}
	if !*apply {
		return nil
	}
	written, err := migrate.Apply(dir, results)
	fmt.Printf("\nwritten: %d (each keeps its V1 manifest as %s)\n", len(written), migrate.BackupName)
	return err
}

func printPlan(results []migrate.Result) {
	ready, skipped := 0, 0
	for _, r := range results {
		state := "BLOCKED"
		switch {
		case r.Ready():
			state, ready = "ready", ready+1
		case r.Skipped != "":
			state, skipped = "skipped", skipped+1
			r.Notes = append(r.Notes, r.Skipped)
		}
		fmt.Printf("%-8s %s\n", state, r.Name)
		for _, b := range r.Blockers {
			fmt.Println("         ✗ " + b)
		}
		for _, n := range r.Notes {
			fmt.Println("         · " + n)
		}
	}
	fmt.Printf("\n%d of %d sites can be converted now.\n", ready, len(results)-skipped)
	if s := migrate.Summary(results); len(s) > 0 {
		fmt.Println("What blocks the rest, by how many sites it would free:")
		fmt.Println("  " + strings.Join(s, "\n  "))
	}
}
