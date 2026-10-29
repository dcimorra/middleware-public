package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/events"
)

func init() {
	subcommands["panel"] = panelCommand
	extraUsage += `  panel open [duration]   lift the panel's address lists and closed password
                          for a while (default 30m), when they shut you out
  panel close             end that early
  panel status            whether it is open, and until when
`
}

// panelCommand is the fire exit. It needs no running supervisor, which is
// the point: it writes data/auth/panel-open, and the running one reads it
// on its next request. Opening is only from the shell; closing is also
// from the panel.
func panelCommand(args []string) error {
	root, rest, err := cliRoot("panel", args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("usage: middleware panel open [duration] | close | status")
	}
	st := auth.New(root, events.New(filepath.Join(root, "data", "events")))
	switch rest[0] {
	case "open":
		d := 30 * time.Minute
		if len(rest) > 1 {
			if d, err = time.ParseDuration(rest[1]); err != nil {
				return fmt.Errorf("duration %q: %w", rest[1], err)
			}
		}
		until, err := st.OpenPanel(d)
		if err != nil {
			return err
		}
		fmt.Printf("The panel's address lists and closed password are lifted until %s.\nSign-in is still required. `middleware panel close` ends it.\n", until.Local().Format("15:04 on 2 Jan"))
		return nil
	case "close":
		if err := st.ClosePanel(); err != nil {
			return err
		}
		fmt.Println("Closed: the panel's rules apply again.")
		return nil
	case "status":
		if u := st.OpenUntil(); !u.IsZero() {
			fmt.Printf("open until %s\n", u.Local().Format(time.RFC3339))
		} else {
			fmt.Println("closed: the panel's rules apply")
		}
		return nil
	}
	return fmt.Errorf("unknown panel command %q", rest[0])
}
