package agents

import (
	"context"
	"encoding/json"
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

// fakeOpenCode lists models the way the CLI does: its own free tier
// always, a provider's only when auth.json has its key; and leaves the
// catalog where the CLI caches it.
func fakeOpenCode(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode")
	script := `#!/bin/sh
if [ "$1" = --version ]; then echo 1.3.17; exit 0; fi
if [ "$1" = models ] && [ "$2" = --refresh ]; then
  mkdir -p "$HOME/.cache/opencode"
  echo '{"openai":{"name":"OpenAI","env":["OPENAI_API_KEY"],"models":{"gpt-4.1":{},"gpt-4o":{}}},"groq":{"name":"Groq","env":["GROQ_API_KEY"],"models":{"llama":{}}}}' > "$HOME/.cache/opencode/models.json"
  echo "Performing one time database migration, may take a few minutes..." >&2
  echo opencode/big-pickle
  if grep -q openai "$HOME/.local/share/opencode/auth.json" 2>/dev/null; then echo openai/gpt-4.1; echo openai/gpt-4o; fi
  if grep -q deepseek "$HOME/.local/share/opencode/auth.json" 2>/dev/null; then echo deepseek/deepseek-chat; echo deepseek/deepseek-reasoner; fi
  exit 0
fi
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConnectingAProviderPutsItsModelsInTheChat(t *testing.T) {
	root := t.TempDir()
	mgr, err := modules.New(root, events.New(""))
	if err != nil {
		t.Fatal(err)
	}
	reg := agent.NewRegistry()
	o := NewOpenCode(reg, filepath.Join(root, "data", "agents", "opencode"))
	o.Store = mgr
	mgr.Register(o)
	bin := fakeOpenCode(t)
	if _, err := mgr.Enable(context.Background(), OpenCodeKey, modules.Config{"bin": bin}, nil); err != nil {
		t.Fatal(err)
	}
	if p, _ := reg.Get("opencode"); p == nil || len(p.Models()) != 0 {
		t.Fatalf("models before any key: %v", p.Models())
	}

	srv := httptest.NewServer(http.StripPrefix("/m", mux(o.Routes())))
	defer srv.Close()
	req, _ := http.NewRequest("PUT", srv.URL+"/m/providers/openai", strings.NewReader(`{"key":"sk-test"}`))
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("connect: %v %v", err, res.Status)
	}
	var st struct {
		Connected []connected       `json:"connected"`
		Models    []string          `json:"models"`
		Catalog   []CatalogProvider `json:"catalog"`
		Err       string            `json:"models_error"`
	}
	_ = json.NewDecoder(res.Body).Decode(&st)
	res.Body.Close()
	if st.Err != "" || len(st.Connected) != 1 || st.Connected[0].Name != "OpenAI" || st.Connected[0].Models != 2 || len(st.Catalog) != 2 {
		t.Fatalf("%+v", st)
	}
	// The free tier the CLI lists is not offered: it only works in its own app.
	if strings.Join(st.Models, ",") != "openai/gpt-4.1,openai/gpt-4o" {
		t.Fatalf("models %v", st.Models)
	}
	p, _ := reg.Get("opencode")
	if strings.Join(p.Models(), ",") != "openai/gpt-4.1,openai/gpt-4o" {
		t.Fatalf("the chat's provider lists %v", p.Models())
	}
	if ok, why := p.Available(); !ok && !strings.Contains(why, "PATH") {
		t.Fatalf("not available with a key: %s", why)
	}
	// Disconnected: its models leave.
	req, _ = http.NewRequest("DELETE", srv.URL+"/m/providers/openai", nil)
	res, _ = http.DefaultClient.Do(req)
	res.Body.Close()
	if p, _ := reg.Get("opencode"); len(p.Models()) != 0 {
		t.Fatalf("models after disconnecting: %v", p.Models())
	}
	// A provider the catalog does not know is refused.
	req, _ = http.NewRequest("PUT", srv.URL+"/m/providers/nobody", strings.NewReader(`{"key":"x"}`))
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 400 {
		t.Fatalf("unknown provider: %d", res.StatusCode)
	}
}

func mux(routes map[string]http.Handler) http.Handler {
	m := http.NewServeMux()
	for p, h := range routes {
		m.Handle(p, h)
	}
	return m
}

// A vendor's module is an agent of its own in the chat, with that vendor's
// models only, and leaves the chat when it is turned off.
func TestAVendorModuleIsAnAgentOfItsOwn(t *testing.T) {
	root := t.TempDir()
	mgr, err := modules.New(root, events.New(""))
	if err != nil {
		t.Fatal(err)
	}
	reg := agent.NewRegistry()
	var ds Vendor
	for _, v := range Vendors {
		if v.Key == "deepseek" {
			ds = v
		}
	}
	o := NewVendor(reg, filepath.Join(root, "data", "agents"), ds)
	o.Store = mgr
	mgr.Register(o)
	if _, err := reg.Get("deepseek"); err == nil {
		t.Fatal("in the chat before being enabled")
	}
	bin := fakeOpenCode(t)
	if _, err := mgr.Enable(context.Background(), "agent-deepseek", modules.Config{"bin": bin, "api_key": "sk-ds"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := o.RefreshModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, err := reg.Get("deepseek")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Models(), ",") != "deepseek/deepseek-chat,deepseek/deepseek-reasoner" {
		t.Fatalf("models %v", p.Models())
	}
	raw, _ := os.ReadFile(filepath.Join(o.Home, ".local/share/opencode/auth.json"))
	if !strings.Contains(string(raw), `"deepseek"`) || !strings.Contains(string(raw), "sk-ds") {
		t.Fatalf("auth %s", raw)
	}
	if err := mgr.Disable(context.Background(), "agent-deepseek"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Get("deepseek"); err == nil {
		t.Fatal("still in the chat after being turned off")
	}
}
