package grok

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/agent"
)

// With GROK_BIN, the real CLI runs a turn as the house starts one, with a
// fake key, and the vendor's refusal is what the turn ends with.
func TestTheRealCLIEndsAFakeKeysTurnWithTheRefusal(t *testing.T) {
	bin := os.Getenv("GROK_BIN")
	if bin == "" {
		t.Skip("GROK_BIN runs it")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	run, err := New(Config{Bin: bin, APIKey: "xai-bogus", Models: []string{"grok-4.7"}}).Start(ctx, agent.Request{Workdir: dir, Prompt: "hola", System: "Be brief.", Stream: dir + "/stream", Jail: agent.Jail{SiteDir: dir, Home: t.TempDir(), Commands: []string{"sudo"}}})
	if err != nil {
		t.Fatal(err)
	}
	evs := agent.Collect(ctx, run)
	last := evs[len(evs)-1]
	if last.Kind != agent.KindError || !strings.Contains(last.Text, "XAI_API_KEY") {
		t.Fatalf("%+v", evs)
	}
}
