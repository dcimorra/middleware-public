package deps

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Installs Grok Build and Antigravity (vendor binaries) and Mistral Vibe (through uv) for
// real into a temporary root, and checks they answer --version. Only with
// DEPS=1: it downloads a couple of hundred megabytes.
func TestTheBinaryAndTheUVToolInstallForReal(t *testing.T) {
	if os.Getenv("DEPS") != "1" {
		t.Skip("DEPS=1 runs it")
	}
	root := t.TempDir()
	m := New(root, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	for _, name := range []string{"grok", "vibe", "agy"} {
		if err := m.Install(ctx, name, func(s string) { t.Log(name, s) }); err != nil {
			t.Fatal(err)
		}
		path, managed, err := m.Find(name)
		if err != nil || !managed || filepath.Dir(path) != m.BinDir() {
			t.Fatalf("%s: %s %v %v", name, path, managed, err)
		}
		if v := m.versionOf(path); v == "" {
			t.Fatalf("%s does not answer --version", name)
		}
		p, _ := m.pkg(name)
		if m.installedVersion(p) == "" {
			t.Fatalf("%s: no version recorded", name)
		}
	}
}

func TestBinaryAndPyPIPackagesSayWhereTheyComeFrom(t *testing.T) {
	m := New(t.TempDir(), nil)
	for _, name := range []string{"grok", "vibe", "agy", "codex", "qwen", "kimi"} {
		if _, ok := m.pkg(name); !ok {
			t.Fatalf("%s is not managed", name)
		}
	}
	if got := m.installedVersion(Package{Name: "grok", Binary: &Binary{}}); got != "" {
		t.Fatalf("a version for nothing installed: %q", got)
	}
}
