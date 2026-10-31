package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/dcimorra/the-middleware/internal/sites"
)

// errInvalid ends `site validate` with a failing status once the report is
// printed: a script (or an agent) that checks before it writes reads the
// exit code, not the words.
var errInvalid = errors.New("the manifest would not work as written")

// validateSite asks the house what a manifest would do without writing
// it. The file is read here and sent: from a site's conversation the CLI
// sees the folder the house sees, and a draft next to site.json is what
// is being checked.
func (e *Env) validateSite(c common, cl *Client, domain, file string) error {
	var body []byte
	switch file {
	case "":
	case "-":
		b, err := io.ReadAll(e.Stdin)
		if err != nil {
			return err
		}
		body = b
	default:
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		body = b
	}
	var raw []byte
	var send any
	if body != nil {
		send = body
	}
	if err := cl.Do("POST", "/api/v1/sites/"+domain+"/validate", send, &raw); err != nil {
		return err
	}
	if c.json {
		e.printJSON(raw)
	}
	var v sites.Validation
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if !c.json {
		e.printValidation(&v)
	}
	if v.Error != "" || !v.OK {
		return errInvalid
	}
	return nil
}

func (e *Env) printValidation(v *sites.Validation) {
	if v.Error != "" {
		fmt.Fprintf(e.Stdout, "refused: %s\n", v.Error)
		return
	}
	if len(v.Routes) > 0 {
		rows := make([][]string, 0, len(v.Routes))
		for _, r := range v.Routes {
			svc := r.Service
			if svc == "" {
				svc = "-"
			}
			state := "ok"
			if r.Conflict != "" {
				state = r.Conflict
			}
			rows = append(rows, []string{r.Host + r.Prefix, svc, state})
		}
		Table(e.Stdout, []string{"NAME", "SERVICE", "CHECK"}, rows)
	}
	for _, p := range v.Problems {
		fmt.Fprintf(e.Stdout, "problem: %s\n", p)
	}
	for _, w := range v.Warnings {
		fmt.Fprintf(e.Stdout, "warning: %s\n", w)
	}
	if v.OK {
		fmt.Fprintln(e.Stdout, "ok: the house would read and run this manifest")
	}
}
