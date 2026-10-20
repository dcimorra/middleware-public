package guard

import (
	"os"
	"strings"
	"testing"
)

// TestNow runs the text rules over everything running on this machine and
// over every log line given in GUARD_LOGS, and prints what would fire.
// GUARD_NOW=1 go test ./internal/guard -run Now -v. Run it before changing
// a rule: a rule is tested by what really runs, not by reading it.
func TestNow(t *testing.T) {
	if os.Getenv("GUARD_NOW") == "" {
		t.Skip("GUARD_NOW=1 to run the rules over this machine")
	}
	procs, err := Reader{}.Processes()
	if err != nil {
		t.Fatal(err)
	}
	fired := 0
	for _, p := range procs {
		if r, where, ok := MatchText(strings.Join(p.Cmdline, " ")); ok {
			fired++
			t.Logf("pid %d %s %s (%s): %.160s", p.PID, r.Level, r.Key, where, strings.Join(p.Cmdline, " "))
		}
	}
	lines := 0
	for _, path := range strings.Fields(os.Getenv("GUARD_LOGS")) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Logf("%s: %v", path, err)
			continue
		}
		for _, l := range strings.Split(string(data), "\n") {
			lines++
			if r, where, ok := MatchLog(l); ok {
				fired++
				t.Logf("%s %s %s (%s): %.160s", path, r.Level, r.Key, where, l)
			}
		}
	}
	t.Logf("%d processes and %d log lines, %d would fire", len(procs), lines, fired)
}
