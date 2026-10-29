package modules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/secrets"
)

// fake is a module whose verify outcome the test decides.
type fake struct {
	key       string
	failWith  string
	installed int
	enabled   int
	disabled  int
	bins      []string
}

func (f *fake) Key() string         { return f.key }
func (f *fake) Name() string        { return "Fake" }
func (f *fake) Description() string { return "a module for tests" }
func (f *fake) Needs() []Need {
	return []Need{{Key: "token", Label: "API token", Secret: true}, {Key: "account", Label: "Account", Optional: true}}
}
func (f *fake) Verify(ctx context.Context, cfg Config) []Line {
	if f.failWith != "" {
		return []Line{OK("token looks like a token"), Fail(f.failWith)}
	}
	return []Line{OK("token valid"), Skip("write permissions not probed")}
}
func (f *fake) Install(ctx context.Context, cfg Config, progress func(string)) error {
	f.installed++
	progress("installed nothing")
	return nil
}
func (f *fake) Enable(ctx context.Context, cfg Config) error {
	f.enabled++
	if cfg["account"] == "" {
		cfg["account"] = "discovered-123"
	}
	return nil
}
func (f *fake) Disable(ctx context.Context) error { f.disabled++; return nil }
func (f *fake) Routes() map[string]http.Handler {
	return map[string]http.Handler{"hook": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})}
}
func (f *fake) Binaries() []string { return f.bins }

func newManager(t *testing.T, mods ...Module) (*Manager, string, *events.Bus) {
	t.Helper()
	t.Setenv(secrets.EnvKey, "")
	root := t.TempDir()
	bus := events.New("")
	m, err := New(root, bus)
	if err != nil {
		t.Fatal(err)
	}
	for _, mod := range mods {
		m.Register(mod)
	}
	return m, root, bus
}

func readStore(t *testing.T, root string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "data", StoreFile))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEnableSavesSecretsEncryptedAndTheEnabledMapInClear(t *testing.T) {
	f := &fake{key: "fake"}
	m, root, bus := newManager(t, f)
	ctx := context.Background()
	lines, err := m.Enable(ctx, "fake", Config{"token": "tok-secret"}, nil)
	if err != nil {
		t.Fatalf("enable: %v (%v)", err, lines)
	}
	if f.installed != 1 || f.enabled != 1 {
		t.Fatalf("install and enable must each run once, got %d/%d", f.installed, f.enabled)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "data", StoreFile))
	if strings.Contains(string(raw), "tok-secret") {
		t.Fatal("the token must not be on disk in clear")
	}
	st := readStore(t, root)
	if st["enabled"].(map[string]any)["fake"] != true {
		t.Fatalf("enabled map must be readable with cat: %s", raw)
	}
	cfg, err := m.Config("fake")
	if err != nil || cfg["token"] != "tok-secret" {
		t.Fatalf("config must decrypt: %v %v", cfg, err)
	}
	if cfg["account"] != "discovered-123" {
		t.Fatalf("what Enable fills in must be saved, got %q", cfg["account"])
	}
	var seen bool
	for _, e := range bus.Recent() {
		if e.Type == "module.enabled" && e.Subject == "fake" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("module.enabled must be emitted")
	}
	info := m.List()[0]
	if !info.Enabled || !info.Configured || !info.Installed {
		t.Fatalf("list: %+v", info)
	}
}

func TestAFailedVerificationWritesNothing(t *testing.T) {
	f := &fake{key: "fake", failWith: "token cannot list zones"}
	m, root, bus := newManager(t, f)
	lines, err := m.Enable(context.Background(), "fake", Config{"token": "bad"}, nil)
	if err != ErrVerifyFailed {
		t.Fatalf("want ErrVerifyFailed, got %v", err)
	}
	if !Failed(lines) || len(lines) != 2 {
		t.Fatalf("lines must be returned so the operator sees which one failed: %v", lines)
	}
	if _, err := os.Stat(filepath.Join(root, "data", StoreFile)); err == nil {
		t.Fatal("a failed verify must not create the store")
	}
	if f.installed != 0 || f.enabled != 0 {
		t.Fatal("nothing must be installed or enabled after a failed verify")
	}
	var seen bool
	for _, e := range bus.Recent() {
		if e.Type == "module.verify_failed" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("module.verify_failed must be emitted")
	}
}

func TestDisableKeepsCredentialsAndForgetDeletesThem(t *testing.T) {
	f := &fake{key: "fake"}
	m, root, _ := newManager(t, f)
	ctx := context.Background()
	if _, err := m.Enable(ctx, "fake", Config{"token": "tok"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Disable(ctx, "fake"); err != nil {
		t.Fatal(err)
	}
	if f.disabled != 1 {
		t.Fatal("the module's Disable must run")
	}
	if m.Enabled("fake") {
		t.Fatal("must be off")
	}
	if cfg, _ := m.Config("fake"); cfg["token"] != "tok" {
		t.Fatal("disable must keep the credentials")
	}
	// Re-enabling with an empty secret keeps the stored one.
	if _, err := m.Enable(ctx, "fake", Config{"token": ""}, nil); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := m.Config("fake"); cfg["token"] != "tok" {
		t.Fatal("an empty secret means keep, not clear")
	}
	if err := m.Forget(ctx, "fake"); err != nil {
		t.Fatal(err)
	}
	st := readStore(t, root)
	if _, ok := st["config"].(map[string]any)["fake"]; ok {
		t.Fatal("forget must delete the config")
	}
	if m.Enabled("fake") || f.disabled != 2 {
		t.Fatal("forget must also disable")
	}
}

func TestTheRunningProcessSeesWhatTheCLIWrote(t *testing.T) {
	t.Setenv(secrets.EnvKey, "")
	root := t.TempDir()
	// Two managers over the same root stand in for the CLI and the
	// supervisor, which are two processes.
	cli, _ := New(root, nil)
	cli.Register(&fake{key: "fake"})
	srv, _ := New(root, nil)
	srv.Register(&fake{key: "fake"})
	if srv.Enabled("fake") {
		t.Fatal("nothing enabled yet")
	}
	if _, err := cli.Enable(context.Background(), "fake", Config{"token": "tok"}, nil); err != nil {
		t.Fatal(err)
	}
	if !srv.Enabled("fake") {
		t.Fatal("the other process must see the store change without a restart")
	}
	cfg, err := srv.Config("fake")
	if err != nil || cfg["token"] != "tok" {
		t.Fatalf("the other process must decrypt with the same key file: %v %v", cfg, err)
	}
}

func TestUnknownFieldsAndMissingRequiredOnesAreRefused(t *testing.T) {
	m, _, _ := newManager(t, &fake{key: "fake"})
	if _, err := m.Verify(context.Background(), "fake", Config{"token": "t", "tokne": "x"}); err == nil {
		t.Fatal("a typo in a field name must be refused")
	}
	if _, err := m.Verify(context.Background(), "fake", Config{}); err == nil {
		t.Fatal("a missing required field must be refused before any probe")
	}
	if _, err := m.Verify(context.Background(), "nope", Config{}); err == nil {
		t.Fatal("an unknown module must be refused")
	}
}

func TestInstalledMeansTheBinariesAreThereAndExecutable(t *testing.T) {
	f := &fake{key: "fake", bins: []string{"cloudflared"}}
	m, _, _ := newManager(t, f)
	if m.Installed("fake") {
		t.Fatal("no binary yet")
	}
	path := filepath.Join(m.BinDir(), "cloudflared")
	os.MkdirAll(m.BinDir(), 0o755)
	os.WriteFile(path, []byte("#!/bin/sh\n"), 0o644)
	if m.Installed("fake") {
		t.Fatal("a file that is not executable is not installed")
	}
	os.Chmod(path, 0o755)
	if !m.Installed("fake") {
		t.Fatal("executable present: installed")
	}
}

func TestInstallBinaryVerifiesTheChecksumAndLeavesNoPartialFile(t *testing.T) {
	body := []byte("#!/bin/sh\necho hi\n")
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()
	dir := t.TempDir()
	ctx := context.Background()
	if _, err := InstallBinary(ctx, dir, "tool", srv.URL+"/tool", "deadbeef", nil); err == nil {
		t.Fatal("a wrong checksum must refuse the install")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("a refused install must leave nothing behind, got %v", entries)
	}
	var steps []string
	path, err := InstallBinary(ctx, dir, "tool", srv.URL+"/tool", hex.EncodeToString(sum[:]), func(s string) { steps = append(steps, s) })
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("want an executable, got %v %v", info, err)
	}
	if len(steps) < 3 {
		t.Fatalf("progress must narrate the steps, got %v", steps)
	}
	if _, err := InstallBinary(ctx, dir, "../evil", srv.URL, "", nil); err == nil {
		t.Fatal("a name with a path in it must be refused")
	}
}

func TestTheHandlerReachesOnlyEnabledModules(t *testing.T) {
	m, _, _ := newManager(t, &fake{key: "fake"})
	h := m.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/fake/hook", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a disabled module's routes must 404, got %d", rec.Code)
	}
	if _, err := m.Enable(context.Background(), "fake", Config{"token": "t"}, nil); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/fake/hook", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want the module's handler, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/fake/other", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown route must 404, got %d", rec.Code)
	}
}

func TestASecretSentEmptyIsKeptAndAnyOtherFieldIsCleared(t *testing.T) {
	f := &fake{key: "fake"}
	m, _, _ := newManager(t, f)
	ctx := context.Background()
	if _, err := m.Enable(ctx, "fake", Config{"token": "tok", "account": "acc-1"}, nil); err != nil {
		t.Fatal(err)
	}
	// The panel never echoes a secret, so empty is "keep it"; a plain
	// field is shown with its value, so empty is the operator's choice.
	if _, err := m.Enable(ctx, "fake", Config{"token": "", "account": ""}, nil); err != nil {
		t.Fatal(err)
	}
	cfg, _ := m.Config("fake")
	if cfg["token"] != "tok" || cfg["account"] != "discovered-123" {
		t.Fatalf("after saving empty: %v", cfg)
	}
}

func TestTheListShowsWhatWasSavedButNeverASecret(t *testing.T) {
	f := &fake{key: "fake"}
	m, _, _ := newManager(t, f)
	if _, err := m.Enable(context.Background(), "fake", Config{"token": "tok-secret", "account": "acc-9"}, nil); err != nil {
		t.Fatal(err)
	}
	// Saved, read back: what the card is drawn from when the operator
	// comes back to it.
	for _, n := range m.List()[0].Needs {
		switch n.Key {
		case "account":
			if n.Value != "acc-9" {
				t.Fatalf("the saved field came back as %q", n.Value)
			}
		case "token":
			if n.Value != "" {
				t.Fatal("a secret was listed")
			}
		}
	}
	// And the module's own copy of its needs was not written into.
	for _, n := range f.Needs() {
		if n.Value != "" {
			t.Fatalf("%s: the module's needs carry a value", n.Key)
		}
	}
}
