package api

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A command that dies says what ended it, not only "signal: killed":
// its context, or a SIGKILL that was not the kernel's out-of-memory killer.
func TestAKilledInstallSaysWhatKilledIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := exec.CommandContext(ctx, "sleep", "5").Run()
	if why := whyKilled(ctx, err, oomKills()); !strings.Contains(why, "cut") {
		t.Fatalf("a context that ended: %q (%v)", why, err)
	}
	err = exec.Command("sh", "-c", "kill -9 $$").Run()
	if why := whyKilled(context.Background(), err, oomKills()); !strings.Contains(why, "SIGKILL") || !strings.Contains(why, "not the kernel") {
		t.Fatalf("a SIGKILL from someone: %q (%v)", why, err)
	}
	if why := whyKilled(context.Background(), exec.Command("false").Run(), oomKills()); why != "" {
		t.Fatalf("an ordinary failure: %q", why)
	}
}
