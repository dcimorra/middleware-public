package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dcimorra/middleware-public/internal/agent"
	"github.com/dcimorra/middleware-public/internal/agent/claude"
	"github.com/dcimorra/middleware-public/internal/agent/gemini"
	"github.com/dcimorra/middleware-public/internal/app"
	"github.com/dcimorra/middleware-public/internal/chat"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/settings"
	"github.com/dcimorra/middleware-public/internal/tasks"
)

// chatManager and taskScheduler are what the api hook reaches for; they
// exist once the hooks are built, before any hook runs.
var (
	chatManager   *chat.Manager
	taskScheduler *tasks.Scheduler
)

func init() {
	hookBuilders = append(hookBuilders,
		hookBuilder{Name: "chat", Order: orderChat, Build: newChatHook},
		hookBuilder{Name: "tasks", Order: orderTasks, Build: newTasksHook},
	)
	subcommands["tasks"] = tasksCmd
	subcommands["chat"] = chatCmd
	extraUsage += `  tasks        list the scheduled tasks with their next run
  tasks run <name>
               run a task now and wait for it
  chat <domain> <text>
               send one message to a site's conversation ("middleware" for the installation's own)
`
}

// providers is the registry every conversation picks from. Credentials
// and binaries come from the agent modules when they are enabled; without
// them the adapters rely on the CLI's own login under the site's HOME.
// There is one registry per process so the agent modules, which replace a
// provider when enabled, and the chat manager, which picks one per turn,
// are looking at the same map.
func providers() *agent.Registry {
	providersOnce.Do(func() {
		providersCur = agent.NewRegistry()
		providersCur.Register(claude.New(claude.Config{}))
		providersCur.Register(gemini.New(gemini.Config{}))
	})
	return providersCur
}

var (
	providersOnce sync.Once
	providersCur  *agent.Registry
)

type chatHook struct{}

func newChatHook(a *app.App) app.Hook {
	chatManager = chat.New(chat.Options{Root: a.Root, Bus: a.Bus, Registry: providers()})
	return chatHook{}
}

func (chatHook) Wrap(next http.Handler) http.Handler       { return next }
func (chatHook) Run(ctx context.Context, a *app.App) error { return nil }

type tasksHook struct{}

func newTasksHook(a *app.App) app.Hook {
	taskScheduler = tasks.NewScheduler(tasks.Options{Root: a.Root, Bus: a.Bus})
	return tasksHook{}
}

func (tasksHook) Wrap(next http.Handler) http.Handler { return next }

func (tasksHook) Run(ctx context.Context, a *app.App) error {
	go taskScheduler.Run(ctx)
	return nil
}

func rootFlag(fs *flag.FlagSet) *string {
	return fs.String("root", "", "installation root (default $MIDDLEWARE_ROOT or /srv/middleware)")
}

func tasksCmd(args []string) error {
	fs := flag.NewFlagSet("tasks", flag.ContinueOnError)
	root := rootFlag(fs)
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	r := settings.Root(*root)
	bus := events.New(filepath.Join(r, "data", "events"))
	s := tasks.NewScheduler(tasks.Options{Root: r, Bus: bus})
	if len(rest) == 0 || rest[0] == "list" {
		list := s.List()
		if len(list) == 0 {
			fmt.Println("no tasks (tasks/<name>.json)")
			return nil
		}
		fmt.Printf("%-24s %-18s %-24s %-20s %s\n", "NAME", "SCHEDULE", "SITE", "NEXT", "LAST")
		for _, t := range list {
			if t.Error != "" {
				fmt.Printf("%-24s %s\n", t.Name, "invalid: "+t.Error)
				continue
			}
			next := "-"
			if t.Disabled {
				next = "disabled"
			} else if !t.Next.IsZero() {
				next = t.Next.Format("2006-01-02 15:04")
			}
			last := "never"
			if t.Last != nil {
				last = fmt.Sprintf("%s %s (%s)", t.Last.At.Format("2006-01-02 15:04"), t.Last.Status, time.Duration(t.Last.Duration).Round(time.Second))
			}
			if t.Running {
				last = "running"
			}
			fmt.Printf("%-24s %-18s %-24s %-20s %s\n", t.Name, t.Schedule, t.Site, next, last)
		}
		return nil
	}
	if rest[0] == "run" {
		if len(rest) != 2 {
			return errors.New("usage: middleware tasks run <name>")
		}
		name := rest[1]
		if err := s.RunNow(name); err != nil {
			fmt.Fprintf(os.Stderr, "see %s\n", tasks.LogPath(r, name))
			return err
		}
		fmt.Printf("%s: ok (log: %s)\n", name, tasks.LogPath(r, name))
		return nil
	}
	return fmt.Errorf("unknown tasks command %q (list, run <name>)", rest[0])
}

// chatCmd runs one turn through the same manager the panel uses, so the
// conversation file, the cost ledger and the jail audit are the same ones,
// and prints what the agent says as it says it.
func chatCmd(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	root := rootFlag(fs)
	model := fs.String("model", "", "model name or alias (default: the site's)")
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(rest) < 2 {
		return errors.New("usage: middleware chat <domain> <text>")
	}
	domain, text := rest[0], strings.Join(rest[1:], " ")
	r := settings.Root(*root)
	bus := events.New(filepath.Join(r, "data", "events"))
	m := chat.New(chat.Options{Root: r, Bus: bus, Registry: providers()})
	ch, unsub := bus.Subscribe()
	defer unsub()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	turnID, err := m.Send(domain, text, *model)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			_ = m.Stop(domain)
			return errors.New("interrupted")
		case ev := <-ch:
			if ev.Subject != domain || ev.Data["turn_id"] != turnID {
				continue
			}
			switch ev.Type {
			case "agent.text":
				fmt.Println(ev.Data["text"])
			case "agent.tool":
				fmt.Fprintf(os.Stderr, "[%s]\n", ev.Data["tool"])
			case "agent.jail_violation":
				fmt.Fprintf(os.Stderr, "jail violation: %v\n", ev.Data["paths"])
			case "agent.done":
				if c, ok := ev.Data["cost"].(float64); ok && c > 0 {
					fmt.Fprintf(os.Stderr, "cost: $%.4f\n", c)
				}
				return nil
			case "agent.error":
				return errors.New(ev.Message)
			}
		}
	}
}
