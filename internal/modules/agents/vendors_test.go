package agents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/agent"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/modules"
)

func vendor(key string) Vendor {
	for _, v := range Vendors {
		if v.Key == key {
			return v
		}
	}
	panic(key)
}

// Each vendor runs its own agent, or Claude Code where it has none.
func TestEachVendorRunsItsOwnAgent(t *testing.T) {
	want := map[string]string{"openai": "codex", "qwen": "qwen", "kimi": "kimi", "xai": "grok", "mistral": "vibe", "deepseek": "claude", "zai": "claude"}
	for key, bin := range want {
		v := vendor(key)
		if v.Bin != bin {
			t.Fatalf("%s runs %s, want %s", key, v.Bin, bin)
		}
		if p := v.Build(BuildArgs{Key: "k", Base: v.BaseURL, Models: v.Defaults}); p.Key() != key {
			t.Fatalf("%s's agent is called %s", key, p.Key())
		}
	}
}

// DeepSeek and Z.AI run Claude Code with their own address and key, never
// the Claude module's credentials.
func TestDeepSeekAndZAIGoToTheirOwnAnthropicCompatibleAPIs(t *testing.T) {
	env := anthropicVendor(vendor("deepseek").BaseURL, "sk-ds", []string{"deepseek-flash"})
	if env["ANTHROPIC_BASE_URL"] != "https://api.deepseek.com/anthropic" || env["ANTHROPIC_AUTH_TOKEN"] != "sk-ds" || env["ANTHROPIC_DEFAULT_SONNET_MODEL"] != "deepseek-flash" {
		t.Fatalf("%v", env)
	}
	if vendor("zai").BaseURL != "https://api.z.ai/api/anthropic" {
		t.Fatal("z.ai's address")
	}
}

// The models come from the vendor's /models with the key, filtered to
// what chats, the defaults first; turned off, the agent leaves the chat.
func TestTheModelsAreTheVendorsAndTheAgentLeavesWhenOff(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Write([]byte(`{"data":[{"id":"grok-imagine-video"},{"id":"grok-4.7"},{"id":"grok-4.6"},{"id":"grok-build-0.1"}]}`))
	}))
	defer srv.Close()
	root := t.TempDir()
	mgr, err := modules.New(root, events.New(""))
	if err != nil {
		t.Fatal(err)
	}
	reg := agent.NewRegistry()
	v := vendor("xai")
	v.ModelsURL = srv.URL
	vm := NewVendor(reg, root+"/data/agents", v)
	vm.Store = mgr
	mgr.Register(vm)
	if _, err := mgr.Enable(context.Background(), "agent-xai", modules.Config{"api_key": "xai-1", "bin": "/bin/true"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := vm.RefreshModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer xai-1" {
		t.Fatalf("auth %q", auth)
	}
	p, err := reg.Get("xai")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Models(), ",") != "grok-4.7,grok-build-0.1,grok-4.6" {
		t.Fatalf("%v", p.Models())
	}
	if err := mgr.Disable(context.Background(), "agent-xai"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Get("xai"); err == nil {
		t.Fatal("still in the chat")
	}
}

// alpha.19 ran these modules with OpenCode: its binary in the config, its
// models and its auth.json (the key, in clear) under the module's HOME.
// None of it survives, and the vendor's own agent runs.
func TestWhatOpenCodeLeftIsForgotten(t *testing.T) {
	homes := t.TempDir()
	old := homes + "/deepseek"
	for _, f := range []string{"models.txt", ".local/share/opencode/auth.json"} {
		if err := os.MkdirAll(filepath.Dir(old+"/"+f), 0o700); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(old+"/"+f, []byte("opencode/big-pickle\n"), 0o600)
	}
	vm := NewVendor(agent.NewRegistry(), homes, vendor("deepseek"))
	if _, err := os.Stat(old + "/.local/share/opencode/auth.json"); err == nil {
		t.Fatal("the old auth.json is still there")
	}
	if strings.Contains(strings.Join(vm.list(), ","), "opencode/") {
		t.Fatalf("old models offered: %v", vm.list())
	}
	if vm.binOf(modules.Config{"bin": "/home/x/.opencode/bin/opencode"}) != "" {
		t.Fatal("OpenCode still runs DeepSeek")
	}
}
