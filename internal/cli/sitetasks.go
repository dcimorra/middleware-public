package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/dcimorra/the-middleware/internal/tasks"
)

const siteTasksUsage = `usage: middleware site tasks <domain> [command]

  list                        the site's tasks with their next and last run (the default)
  save <name> <file.json|->   write a task from a file or stdin; its site is this one
  rm <name>                   delete it; its history and log stay
  run <name>                  start a run now; its output is in log
  stop <name>                 stop a run in progress
  log <name> [--lines 500]    the last lines of its output
`

// siteTasks is `middleware site tasks`: a site's own tasks through
// /sites/{domain}/tasks, which is all a site's agent turn may reach on
// the socket. It does not read tasks/ itself as `middleware tasks` does:
// a turn's jail denies that folder, and the API is where "this task is
// another site's" is decided, once.
func (e *Env) siteTasks(c common, cl *Client, domain string, args []string, lines int) error {
	verb := "list"
	if len(args) > 0 {
		verb, args = args[0], args[1:]
	}
	base := "/api/v1/sites/" + url.PathEscape(domain) + "/tasks"
	name := func(want int) (string, error) {
		if len(args) != want {
			fmt.Fprint(e.Stderr, siteTasksUsage)
			if want == 2 {
				return "", fmt.Errorf("site tasks %s: a task name and a file (- for stdin) are required", verb)
			}
			return "", fmt.Errorf("site tasks %s: a task name is required", verb)
		}
		if !tasks.ValidName(args[0]) {
			return "", fmt.Errorf("site tasks %s: %q is not a task name", verb, args[0])
		}
		return args[0], nil
	}
	switch verb {
	case "list", "ls":
		if len(args) != 0 {
			fmt.Fprint(e.Stderr, siteTasksUsage)
			return fmt.Errorf("site tasks list: unexpected %q", args[0])
		}
		var raw []byte
		if err := cl.Do("GET", base, nil, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var list []tasks.Status
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintf(e.Stdout, "no tasks for %s\n", domain)
			return nil
		}
		Table(e.Stdout, []string{"NAME", "SCHEDULE", "NEXT", "LAST"}, taskRows(list))
		return nil
	case "save", "put":
		n, err := name(2)
		if err != nil {
			return err
		}
		var data []byte
		if args[1] == "-" {
			data, err = io.ReadAll(e.Stdin)
		} else {
			data, err = os.ReadFile(args[1])
		}
		if err != nil {
			return err
		}
		var raw []byte
		if err := cl.Do("PUT", base+"/"+n, data, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var st tasks.Status
		_ = json.Unmarshal(raw, &st)
		fmt.Fprintf(e.Stdout, "Saved %s for %s: next %s. Try it now with `middleware site tasks %s run %s`.\n", n, domain, nextRun(st), domain, n)
		return nil
	case "rm", "remove", "delete":
		n, err := name(1)
		if err != nil {
			return err
		}
		return e.simple(c, cl, "DELETE", base+"/"+n, nil, "Deleted "+n+"; its history and log stay")
	case "run":
		n, err := name(1)
		if err != nil {
			return err
		}
		return e.simple(c, cl, "POST", base+"/"+n+"/run", nil, "Started "+n+"; `middleware site tasks "+domain+" log "+n+"` shows its output, `list` how it ended")
	case "stop":
		n, err := name(1)
		if err != nil {
			return err
		}
		return e.simple(c, cl, "POST", base+"/"+n+"/stop", nil, "Stopped "+n)
	case "log", "logs":
		n, err := name(1)
		if err != nil {
			return err
		}
		q := ""
		if lines > 0 {
			q = "?lines=" + strconv.Itoa(lines)
		}
		var raw []byte
		if err := cl.Do("GET", base+"/"+n+"/log"+q, nil, &raw); err != nil {
			return err
		}
		_, err = e.Stdout.Write(raw)
		return err
	}
	fmt.Fprint(e.Stderr, siteTasksUsage)
	return fmt.Errorf("site tasks: unknown command %q", verb)
}

// taskRows are the lines `middleware tasks` prints, without the site
// column, which here would say the same domain on every line.
func taskRows(list []tasks.Status) [][]string {
	rows := make([][]string, 0, len(list))
	for _, t := range list {
		if t.Error != "" {
			rows = append(rows, []string{t.Name, "invalid: " + oneLine(t.Error)})
			continue
		}
		last := "never"
		if t.Last != nil {
			last = fmt.Sprintf("%s %s (%s)", t.Last.At.Local().Format("2006-01-02 15:04"), t.Last.Status, time.Duration(t.Last.Duration).Round(time.Second))
		}
		if t.Running {
			last = "running"
		}
		rows = append(rows, []string{t.Name, t.Schedule, nextRun(t), last})
	}
	return rows
}

func nextRun(t tasks.Status) string {
	switch {
	case t.Disabled:
		return "disabled"
	case t.Next.IsZero():
		return "-"
	}
	return t.Next.Local().Format("2006-01-02 15:04")
}
