package agents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/modules"
)

func TestClaudesUsageIsAskedWithTheSubscriptionsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-ant-oat01-x" || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"five_hour":{"utilization":12.5,"resets_at":"2026-10-23T18:00:00+00:00"},"seven_day":{"utilization":40,"resets_at":"2026-10-27T09:00:00Z"},"seven_day_opus":null}`))
	}))
	defer srv.Close()
	old := ClaudeUsageURL
	ClaudeUsageURL = srv.URL
	defer func() { ClaudeUsageURL = old }()

	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "data"), 0o755)
	mgr, err := modules.New(root, events.New(""))
	if err != nil {
		t.Fatal(err)
	}
	m := Claude(agent.NewRegistry())
	m.Store, m.Usage = mgr, agent.OpenUsageBook(filepath.Join(root, "usage.json"))
	mgr.Register(m)
	cfg := modules.Config{"oauth_token": "sk-ant-oat01-x"}
	out := m.usageState(context.Background(), cfg, true)
	u, _ := out["usage"].(agent.Usage)
	if out["error"] != nil || len(u.Windows) != 2 || u.Windows[0].Name != "5h" || u.Windows[0].Used != 12.5 || u.Windows[0].ResetsAt.Hour() != 18 || u.Source != "vendor" {
		t.Fatalf("%+v", out)
	}
	// A refusal (the scope a setup-token lacks) is said, and what the
	// book had stays.
	out = m.usageState(context.Background(), modules.Config{"oauth_token": "sk-ant-oat01-other"}, true)
	if out["error"] == nil || out["usage"] == nil {
		t.Fatalf("%+v", out)
	}
	// With an API key there is no subscription to ask about.
	if out := m.usageState(context.Background(), modules.Config{"oauth_token": "t", "api_key": "k"}, true); out["supported"] != false {
		t.Fatalf("%+v", out)
	}
}

func TestCodexsUsageIsAskedWithTheTokenCodexRefreshedLast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fresh" || r.Header.Get("ChatGPT-Account-Id") != "acc" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"plan_type":"plus","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":7,"limit_window_seconds":18000,"reset_after_seconds":3600,"reset_at":1790000000},"secondary_window":{"used_percent":55,"limit_window_seconds":604800,"reset_after_seconds":86400,"reset_at":1790500000}}}`))
	}))
	defer srv.Close()
	old := CodexUsageURL
	CodexUsageURL = srv.URL
	defer func() { CodexUsageURL = old }()

	root := t.TempDir()
	homes := filepath.Join(root, "agents")
	var openai Vendor
	for _, v := range Vendors {
		if v.Key == "openai" {
			openai = v
		}
	}
	vm := NewVendor(agent.NewRegistry(), homes, openai)
	vm.Usage = agent.OpenUsageBook(filepath.Join(root, "usage.json"))
	os.MkdirAll(filepath.Join(vm.Home, "codex-home"), 0o700)
	os.WriteFile(filepath.Join(vm.Home, "codex-home", "auth.json"), []byte(`{"tokens":{"access_token":"fresh","account_id":"acc","refresh_token":"r"}}`), 0o600)
	out := vm.usageState(context.Background(), modules.Config{"subscription": `{"tokens":{"access_token":"stale","account_id":"acc","refresh_token":"r"}}`}, true)
	u, _ := out["usage"].(agent.Usage)
	if out["error"] != nil || u.Plan != "plus" || len(u.Windows) != 2 || u.Windows[0].Name != "5h" || u.Windows[0].Used != 7 || u.Windows[1].Name != "7d" || u.Windows[1].ResetsAt.Unix() != 1790500000 {
		t.Fatalf("%+v", out)
	}
}
