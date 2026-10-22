package qwen

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/agent"
)

// With QWEN_BIN, the real CLI runs a turn as the house starts one, with a
// fake key, and the vendor's refusal is what the turn ends with.
func TestTheRealCLIEndsAFakeKeysTurnWithTheRefusal(t *testing.T) {
	bin := os.Getenv("QWEN_BIN")
	if bin == "" {
		t.Skip("QWEN_BIN runs it")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	run, err := New(Config{Bin: bin, APIKey: "sk-bogus", Models: []string{"qwen3.8-max"}}).Start(ctx, agent.Request{Workdir: dir, Prompt: "hola", System: "Be brief.", Stream: dir + "/stream", Jail: agent.Jail{SiteDir: dir, Home: t.TempDir(), Commands: []string{"sudo"}}})
	if err != nil {
		t.Fatal(err)
	}
	evs := agent.Collect(ctx, run)
	last := evs[len(evs)-1]
	if last.Kind != agent.KindError || !strings.Contains(last.Text, "401") {
		t.Fatalf("%+v", evs)
	}
}
