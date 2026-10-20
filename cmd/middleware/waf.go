package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dcimorra/middleware-public/internal/access"
	"github.com/dcimorra/middleware-public/internal/app"
	"github.com/dcimorra/middleware-public/internal/edge"
	"github.com/dcimorra/middleware-public/internal/engine"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/guard"
	"github.com/dcimorra/middleware-public/internal/proxy"
	"github.com/dcimorra/middleware-public/internal/settings"
	"github.com/dcimorra/middleware-public/internal/traffic"
	"github.com/dcimorra/middleware-public/internal/waf"
)

// The request chain (wire.go has the order) is inbound → waf → access →
// traffic → proxy: the WAF refuses the abusive before anything else spends
// on them, access closes the door the operator closed, and traffic counts
// whatever came out, error pages included. The guard wraps nothing; it
// only runs. These are the instances the API routes talk to (block,
// policy, incidents), set when the app starts.
var (
	theWAF     *waf.WAF
	theAccess  *access.Access
	theTraffic *traffic.Recorder
	theGuard   *guard.Guard
)

func init() {
	hookBuilders = append(hookBuilders,
		hookBuilder{Name: "waf", Order: orderWAF, Build: buildWAF},
		hookBuilder{Name: "access", Order: orderAccess, Build: buildAccess},
		hookBuilder{Name: "traffic", Order: orderTraffic, Build: buildTraffic},
		hookBuilder{Name: "guard", Order: orderGuard, Build: buildGuard},
	)
	subcommands["waf"] = wafCommand
	subcommands["guard"] = guardCommand
	extraUsage += `  waf block <ip> [duration] [reason]   block an address (default one day)
  waf unblock <ip>                     remove an address from the block list
  waf list                             show the block list
  guard incidents                      show what the guard found
`
}

// hook adapts a package with Wrap and Run(ctx) to app.Hook.
type hook struct {
	wrap func(http.Handler) http.Handler
	run  func(ctx context.Context)
}

func (h hook) Wrap(next http.Handler) http.Handler {
	if h.wrap == nil {
		return next
	}
	return h.wrap(next)
}

func (h hook) Run(ctx context.Context, _ *app.App) error {
	if h.run != nil {
		h.run(ctx)
	}
	return nil
}

// edgeCountry is the country the edge says, which is all the house knows
// without a GeoIP database. It was never wired before, so every
// deny_countries rule in the WAF and in manifests was dead configuration.
func edgeCountry(r *http.Request, _ net.IP) string { return edge.Country(r) }

func dataDir(a *app.App) string { return filepath.Join(a.Root, "data") }

func buildWAF(a *app.App) app.Hook {
	w, err := waf.New(waf.Options{
		DataDir: dataDir(a),
		Bus:     a.Bus,
		Country: edgeCountry,
		Lookup: func(host string) (waf.Site, bool) {
			sn, ok := a.Engine.Lookup(host)
			if !ok {
				return waf.Site{}, false
			}
			s := waf.Site{Domain: sn.Domain, Dir: sn.Dir}
			if sn.Manifest != nil {
				s.WAF = sn.Manifest.WAF
			}
			return s, true
		},
	})
	var pe *waf.PolicyError
	switch {
	case errors.As(err, &pe):
		// A typo in policy.json must not take every site down with it:
		// the WAF runs on defaults and the event says what to fix.
		a.Bus.Emit(events.Event{Type: "waf.error", Actor: "engine", Level: "error", Message: "running with the default policy: " + err.Error()})
	case err != nil:
		a.Bus.Emit(events.Event{Type: "waf.error", Actor: "engine", Level: "error", Message: "waf not started: " + err.Error()})
		return nil
	}
	theWAF = w
	return hook{wrap: w.Wrap, run: w.Run}
}

func buildAccess(a *app.App) app.Hook {
	lookup := func(host string) (access.Site, bool) {
		sn, ok := a.Engine.Lookup(host)
		if !ok || sn.Manifest == nil {
			return access.Site{}, false
		}
		return access.Site{Domain: sn.Domain, Access: sn.Manifest.Access}, true
	}
	ac, err := access.New(access.Options{
		DataDir:  dataDir(a),
		ClientIP: edge.ClientIP,
		Country:  edgeCountry,
		Lookup:   lookup,
	})
	if err != nil {
		a.Bus.Emit(events.Event{Type: "access.error", Actor: "engine", Level: "error", Message: "session key not available, protected sites are closed: " + err.Error()})
		return hook{wrap: access.Closed(lookup)}
	}
	theAccess = ac
	return hook{wrap: ac.Wrap}
}

func buildTraffic(a *app.App) app.Hook {
	r := traffic.New(traffic.Options{
		LogDir:   filepath.Join(dataDir(a), "logs"),
		ClientIP: edge.ClientIP,
		Host:     proxy.Host,
	})
	theTraffic = r
	return hook{wrap: r.Wrap, run: r.Run}
}

func buildGuard(a *app.App) app.Hook {
	g := guard.New(guard.Options{
		ProcRoot:  "/proc",
		DataDir:   dataDir(a),
		SitesDir:  filepath.Join(a.Root, "sites"),
		PortRange: [2]int{a.Settings.Ports.From, a.Settings.Ports.To},
		Bus:       a.Bus,
		Sites: func() []guard.SiteInfo {
			snaps := a.Engine.Sites()
			out := make([]guard.SiteInfo, 0, len(snaps))
			for _, sn := range snaps {
				out = append(out, guardSite(sn))
			}
			return out
		},
	})
	theGuard = g
	return hook{run: g.Run}
}

func guardSite(sn engine.Snapshot) guard.SiteInfo {
	s := guard.SiteInfo{Domain: sn.Domain, Dir: sn.Dir}
	if sn.Port > 0 {
		s.Ports = append(s.Ports, sn.Port)
	}
	for _, w := range sn.Workers {
		if w.Port > 0 {
			s.Ports = append(s.Ports, w.Port)
		}
	}
	return s
}

// --- CLI ---

// cliRoot parses the shared -root flag, wherever it sits (`waf list
// --root /x` reads as naturally as the flag first), and returns the
// remaining args. The first smoke run of the binary had every `--root`
// after the verb silently ignored and the commands answering about
// /srv/middleware.
func cliRoot(name string, args []string) (string, []string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	root := fs.String("root", "", "installation root (default $MIDDLEWARE_ROOT or /srv/middleware)")
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return "", nil, err
	}
	return settings.Root(*root), rest, nil
}

// openWAF opens the block list from disk. The running supervisor notices
// the file change within seconds, so the CLI needs no socket to the API.
func openWAF(root string) (*waf.WAF, error) {
	return waf.New(waf.Options{DataDir: filepath.Join(root, "data"), Bus: events.New(filepath.Join(root, "data", "events"))})
}

func wafCommand(args []string) error {
	root, rest, err := cliRoot("waf", args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("usage: middleware waf block|unblock|list")
	}
	w, err := openWAF(root)
	if err != nil {
		return err
	}
	switch rest[0] {
	case "block":
		if len(rest) < 2 {
			return errors.New("usage: middleware waf block <ip> [duration] [reason]")
		}
		ip := net.ParseIP(rest[1])
		if ip == nil {
			return fmt.Errorf("%q is not an address", rest[1])
		}
		dur := 24 * time.Hour
		reason := "blocked from the command line"
		if len(rest) > 2 {
			d, err := time.ParseDuration(rest[2])
			if err != nil {
				return fmt.Errorf("duration %q: %w", rest[2], err)
			}
			dur = d
		}
		if len(rest) > 3 {
			reason = strings.Join(rest[3:], " ")
		}
		if err := w.Block(ip, dur, reason); err != nil {
			return err
		}
		fmt.Printf("blocked %s for %s\n", ip, dur)
		return nil
	case "unblock":
		if len(rest) < 2 {
			return errors.New("usage: middleware waf unblock <ip>")
		}
		ip := net.ParseIP(rest[1])
		if ip == nil {
			return fmt.Errorf("%q is not an address", rest[1])
		}
		if err := w.Unblock(ip); err != nil {
			return err
		}
		fmt.Printf("unblocked %s\n", ip)
		return nil
	case "list":
		entries := w.Blocked()
		if len(entries) == 0 {
			fmt.Println("nothing blocked")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "ADDRESS\tUNTIL\tSTRIKES\tREASON")
		for _, e := range entries {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", e.IP, e.Until.Local().Format(time.RFC3339), e.Strikes, e.Reason)
		}
		return tw.Flush()
	default:
		return fmt.Errorf("unknown waf command %q", rest[0])
	}
}

func guardCommand(args []string) error {
	root, rest, err := cliRoot("guard", args)
	if err != nil {
		return err
	}
	if len(rest) == 0 || rest[0] != "incidents" {
		return errors.New("usage: middleware guard incidents")
	}
	incidents, err := guard.ReadIncidents(filepath.Join(root, "data"))
	if err != nil {
		return err
	}
	if len(incidents) == 0 {
		fmt.Println("no incidents")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "AT\tDOMAIN\tLEVEL\tRULE\tWHERE\tACTION\tID")
	for _, inc := range incidents {
		if inc.Dismissed {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", inc.At.Local().Format(time.RFC3339), inc.Domain, inc.Level, inc.Rule, inc.Where, inc.Action, inc.ID)
	}
	return tw.Flush()
}
