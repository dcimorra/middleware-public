package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/dcimorra/middleware-public/internal/app"
	"github.com/dcimorra/middleware-public/internal/deps"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/modules"
	"github.com/dcimorra/middleware-public/internal/modules/cloudflare"
	"github.com/dcimorra/middleware-public/internal/modules/github"
	"github.com/dcimorra/middleware-public/internal/modules/openspec"
	"github.com/dcimorra/middleware-public/internal/settings"
)

func init() {
	hookBuilders = append(hookBuilders, hookBuilder{Name: "modules", Order: orderModules, Build: func(a *app.App) app.Hook { return modulesHook{} }})
	subcommands["modules"] = modulesCommand
	extraUsage += `  modules                        list modules and their state
  modules verify <key> k=v…      probe credentials without saving them
  modules enable <key> k=v…      verify, install and save
  modules disable <key>          turn off, keep credentials
  modules forget <key>           turn off and delete credentials
`
}

// moduleSet is the one registry the process has: the hook builds it at
// start and the api reaches it through modulesFor. Two registries would
// each read the store and disagree on what is enabled.
type moduleSet struct {
	Manager    *modules.Manager
	Cloudflare *cloudflare.Module
	GitHub     *github.Module
	Deps       *deps.Manager
	OpenSpec   *openspec.Module
}

var (
	modulesOnce sync.Once
	modulesCur  *moduleSet
	modulesErr  error
)

// modulesFor returns the registry for the running app, building it on
// first use. The github module's deploys call the engine's Restart when a
// manifest has no pointer, and the cloudflare module's tunnel routes point
// at the loopback listener.
func modulesFor(a *app.App) (*moduleSet, error) {
	modulesOnce.Do(func() {
		modulesCur, modulesErr = buildModules(a.Root, a.Bus, a.Engine.Restart, "http://"+a.Settings.Listen)
	})
	return modulesCur, modulesErr
}

func buildModules(root string, bus *events.Bus, restart func(context.Context, string) error, service string) (*moduleSet, error) {
	mgr, err := modules.New(root, bus)
	if err != nil {
		return nil, err
	}
	dm := deps.New(root, bus)
	dm.AutoUpdate = func() bool {
		s, err := settings.Load(root)
		return err == nil && s.Deps.AutoUpdateOn()
	}
	cf := cloudflare.New(mgr)
	cf.Service = service
	cf.Connector = &cloudflare.Connector{Root: root, Bus: bus, Want: func() bool {
		cfg, err := mgr.Config(cloudflare.Key)
		return err == nil && mgr.Enabled(cloudflare.Key) && cfg["tunnel_id"] != ""
	}}
	gh := github.New(mgr, bus, restart)
	osp := openspec.New(dm)
	mgr.Register(cf)
	mgr.Register(gh)
	mgr.Register(osp)
	// The VPN and the agent modules are built in wire.go: they need the
	// helper, the secrets box and the provider registry, which this file
	// does not know about.
	if err := registerExtraModules(mgr, root, bus, dm); err != nil {
		return nil, err
	}
	return &moduleSet{Manager: mgr, Cloudflare: cf, GitHub: gh, Deps: dm, OpenSpec: osp}, nil
}

// modulesHook builds the registry at start so a bad secret key fails the
// start rather than the first request. It mounts nothing: the api owns
// /api/v1/modules/ and its authentication.
type modulesHook struct{}

func (modulesHook) Wrap(next http.Handler) http.Handler { return next }

func (modulesHook) Run(ctx context.Context, a *app.App) error {
	set, err := modulesFor(a)
	if err != nil {
		return err
	}
	go set.Cloudflare.Connector.Run(ctx)
	return nil
}

// modulesCommand is the CLI. It works on the store directly; the running
// process re-reads the store on its next look, so no socket is needed.
func modulesCommand(args []string) error {
	// -root may come anywhere: `modules enable github -root /x token=…`
	// reads as naturally as the flag first, and the flag package stops at
	// the first bare word.
	rootFlag, rest := takeRoot(args)
	root := settings.Root(rootFlag)
	s, err := settings.Load(root)
	if err != nil {
		return err
	}
	set, err := buildModules(root, events.New(filepath.Join(root, "data", "events")), nil, "http://"+s.Listen)
	if err != nil {
		return err
	}
	mgr := set.Manager
	if len(rest) == 0 || (len(rest) == 1 && rest[0] == "list") {
		return listModules(mgr)
	}
	verb := rest[0]
	if len(rest) < 2 {
		return fmt.Errorf("modules %s: which module? one of %s", verb, strings.Join(mgr.Keys(), ", "))
	}
	key := rest[1]
	cfg, err := parseConfig(rest[2:])
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch verb {
	case "verify":
		lines, err := mgr.Verify(ctx, key, cfg)
		printLines(lines)
		if err != nil {
			return err
		}
		if modules.Failed(lines) {
			return errors.New("verification failed; nothing was saved")
		}
		fmt.Println("ok: nothing saved (use `modules enable` to save)")
		return nil
	case "enable":
		lines, err := mgr.Enable(ctx, key, cfg, func(s string) { fmt.Println("  …", s) })
		printLines(lines)
		if err != nil {
			return err
		}
		fmt.Printf("%s enabled\n", key)
		if key == github.Key {
			saved, _ := mgr.Config(key)
			fmt.Printf("webhook: POST /api/v1/modules/github/hook on the panel domain, secret %s\n", saved["webhook_secret"])
		}
		return nil
	case "disable":
		if len(cfg) > 0 {
			return errors.New("modules disable takes no k=v")
		}
		if err := mgr.Disable(ctx, key); err != nil {
			return err
		}
		fmt.Printf("%s disabled (credentials kept; `modules forget %s` deletes them)\n", key, key)
		return nil
	case "forget":
		if len(cfg) > 0 {
			return errors.New("modules forget takes no k=v")
		}
		if err := mgr.Forget(ctx, key); err != nil {
			return err
		}
		fmt.Printf("%s forgotten\n", key)
		return nil
	default:
		return fmt.Errorf("modules: unknown verb %q (verify, enable, disable, forget)", verb)
	}
}

// takeRoot pulls -root/--root (with = or a following value) out of args.
func takeRoot(args []string) (root string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-root" || a == "--root":
			if i+1 < len(args) {
				root = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "-root=") || strings.HasPrefix(a, "--root="):
			root = a[strings.Index(a, "=")+1:]
		default:
			rest = append(rest, a)
		}
	}
	return root, rest
}

func listModules(mgr *modules.Manager) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\tSTATE\tINSTALLED\tNEEDS\tDESCRIPTION")
	for _, info := range mgr.List() {
		state := "off"
		switch {
		case info.Enabled:
			state = "on"
		case info.Configured:
			state = "off (configured)"
		}
		installed := "yes"
		if !info.Installed {
			installed = "no"
		}
		needs := make([]string, 0, len(info.Needs))
		for _, n := range info.Needs {
			s := n.Key
			if n.Optional {
				s += "?"
			}
			needs = append(needs, s)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", info.Key, state, installed, strings.Join(needs, ","), info.Description)
	}
	return w.Flush()
}

// parseConfig reads k=v arguments; a value of "@file" or "-" reads the
// value from a file or stdin so a token never sits in the shell history.
func parseConfig(args []string) (modules.Config, error) {
	cfg := modules.Config{}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("want key=value, got %q", a)
		}
		switch {
		case v == "-":
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return nil, err
			}
			v = strings.TrimSpace(string(data))
		case strings.HasPrefix(v, "@"):
			data, err := os.ReadFile(v[1:])
			if err != nil {
				return nil, err
			}
			v = strings.TrimSpace(string(data))
		}
		cfg[k] = v
	}
	return cfg, nil
}

func printLines(lines []modules.Line) {
	for _, l := range lines {
		mark := "✓"
		switch l.Status {
		case modules.StatusSkip:
			mark = "–"
		case modules.StatusFail:
			mark = "✗"
		}
		fmt.Printf("  %s %s\n", mark, l.Text)
	}
}
