package guard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
)

// The browser reaper. A headless Chromium started by a site, a test run,
// a conversation or the panel's own Browser is reparented to init when its
// owner dies, and then sits there with a gigabyte. Three things learned:
//
//   - Thirty minutes, not ten: a scraping job or a long test keeps one
//     browser for a while on purpose, and killing it mid-run is the harm.
//   - A browser someone is connected to is not an orphan, whoever its
//     parent is: a CDP client on its debugging port is using it.
//   - A conversation's agent opens browsers and forgets them; past a cap
//     per conversation the oldest go, parent or not.
//
// Every kill is a line in data/guard/reaper.jsonl, so "where did my
// browser go" has an answer.

// MaxConversationBrowsers is how many browsers one agent conversation
// may keep open at once.
const MaxConversationBrowsers = 3

// browserMain says whether p is a browser's main process: a browser whose
// parent is not one (renderers and GPU processes hang below it).
func browserMain(p *Process, byPID map[int]*Process) bool {
	if !isBrowser(p) {
		return false
	}
	parent := byPID[p.PPID]
	return parent == nil || !isBrowser(parent)
}

func debugPort(p *Process) int {
	for _, a := range p.Cmdline {
		if v, ok := strings.CutPrefix(a, "--remote-debugging-port="); ok {
			n, _ := strconv.Atoi(v)
			return n
		}
	}
	return 0
}

// agentComms are the agent CLIs by process name.
var agentComms = map[string]bool{"claude": true, "agy": true, "antigravity": true, "opencode": true, "codex": true, "qwen": true, "kimi": true, "grok": true, "vibe": true, "vibe-rs": true}

func agentPathIn(hay string) bool {
	for name := range agentComms {
		if strings.Contains(hay, "/bin/"+name+" ") || strings.HasSuffix(hay, "/bin/"+name) {
			return true
		}
	}
	return false
}

// agentOf is the agent CLI process a browser descends from, or 0.
func agentOf(p *Process, byPID map[int]*Process) int {
	for depth, q := 0, byPID[p.PPID]; depth < 32 && q != nil; depth, q = depth+1, byPID[q.PPID] {
		hay := strings.ToLower(q.Comm + " " + strings.Join(q.Cmdline, " "))
		if agentComms[q.Comm] || strings.Contains(hay, "@anthropic-ai/claude-code") || strings.Contains(hay, "@openai/codex") || strings.Contains(hay, "@qwen-code/") || strings.Contains(hay, "@moonshot-ai/kimi-code") || agentPathIn(hay) {
			return q.PID
		}
	}
	return 0
}

type reaped struct {
	At     time.Time `json:"at"`
	PID    int       `json:"pid"`
	Comm   string    `json:"comm"`
	Reason string    `json:"reason"`
	Age    string    `json:"age"`
	Killed int       `json:"killed"`
	Failed int       `json:"failed"`
	Cmd    string    `json:"cmd,omitempty"`
}

func (g *Guard) reap(p *Process, byPID map[int]*Process, reason string) {
	now := g.now()
	killed, failed := g.killTree(p.PID, byPID, map[int]bool{})
	rec := reaped{At: now.UTC(), PID: p.PID, Comm: p.Comm, Reason: reason, Age: now.Sub(p.Started).Round(time.Second).String(), Killed: killed, Failed: failed}
	if c := strings.Join(p.Cmdline, " "); len(c) > 300 {
		rec.Cmd = c[:300] + "…"
	} else {
		rec.Cmd = c
	}
	if line, err := json.Marshal(rec); err == nil {
		path := filepath.Join(g.opts.DataDir, "guard", "reaper.jsonl")
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, _ = f.Write(append(line, '\n'))
			f.Close()
		}
	}
	g.emit(events.Event{Type: "guard.orphan", Actor: "guard", Level: "warn",
		Message: fmt.Sprintf("browser %s (pid %d, %s): %s; killed %d, failed %d", p.Comm, p.PID, rec.Age, reason, killed, failed),
		Data:    map[string]any{"pid": p.PID, "comm": p.Comm, "reason": reason}})
}

// huntBrowsers is the reaper's pass.
func (g *Guard) huntBrowsers(procs []Process, byPID map[int]*Process, claimed map[int]bool) {
	now := g.now()
	var inUse map[int]bool
	byAgent := map[int][]*Process{}
	for i := range procs {
		p := &procs[i]
		if !browserMain(p, byPID) {
			continue
		}
		if a := agentOf(p, byPID); a != 0 {
			byAgent[a] = append(byAgent[a], p)
		}
		orphan := !claimed[p.PID] && (p.PPID <= 1 || byPID[p.PPID] == nil)
		if !orphan || p.Started.IsZero() || now.Sub(p.Started) < g.opts.OrphanAge {
			continue
		}
		if port := debugPort(p); port != 0 {
			if inUse == nil {
				inUse = g.reader.connected()
			}
			if inUse[port] {
				continue
			}
		}
		if g.recently("orphan|" + strconv.Itoa(p.PID)) {
			continue
		}
		g.reap(p, byPID, fmt.Sprintf("orphaned and unused for over %s", g.opts.OrphanAge))
	}
	for _, list := range byAgent {
		if len(list) <= MaxConversationBrowsers {
			continue
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Started.After(list[j].Started) })
		for _, p := range list[MaxConversationBrowsers:] {
			if g.recently("cap|" + strconv.Itoa(p.PID)) {
				continue
			}
			g.reap(p, byPID, fmt.Sprintf("a conversation may keep %d browsers open; this was the oldest", MaxConversationBrowsers))
		}
	}
}
