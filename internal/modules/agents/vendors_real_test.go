package agents

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/agent"
)

// With CLAUDE_BIN, Claude Code runs a DeepSeek turn and a Z.AI turn with
// fake keys: the refusals come from those vendors, which is the proof the
// turn went to them and not to Anthropic.
func TestClaudeCodeTakesDeepSeekAndZAIToTheirAPIs(t *testing.T) {
	bin := os.Getenv("CLAUDE_BIN")
	if bin == "" {
		t.Skip("CLAUDE_BIN runs it")
	}
	for _, key := range []string{"deepseek", "zai"} {
		v := vendor(key)
		p := v.Build("sk-bogus-0000000000", v.BaseURL, bin, v.Defaults)
		dir := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		run, err := p.Start(ctx, agent.Request{Workdir: dir, Prompt: "hola", Stream: dir + "/stream", Jail: agent.Jail{SiteDir: dir, Home: t.TempDir()}})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		evs := agent.Collect(ctx, run)
		cancel()
		last := evs[len(evs)-1]
		t.Logf("%s: %s", key, last.Text)
		if last.Kind != agent.KindError || !strings.Contains(last.Text, "401") || strings.Contains(strings.ToLower(last.Text), "anthropic.com") {
			t.Fatalf("%s: %+v", key, evs)
		}
	}
}
