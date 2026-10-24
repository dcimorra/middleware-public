package nodeenv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheVersionASiteWantsIsReadFromWhereNodeProjectsSayIt(t *testing.T) {
	for _, c := range []struct{ file, body, want string }{
		{".nvmrc", "v18.19.0\n", "18"},
		{".nvmrc", "lts/iron", "lts"},
		{".node-version", "20", "20"},
		{"package.json", `{"engines":{"node":">=20.1 <23"}}`, "20"},
		{"package.json", `{"engines":{"node":"^22"}}`, "22"},
		{"package.json", `{"name":"x"}`, ""},
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, c.file), []byte(c.body), 0o644)
		if got := Wanted(dir); got != c.want {
			t.Errorf("%s %q: %q, want %q", c.file, c.body, got, c.want)
		}
	}
}

func TestACommandNeedsNodeWhenItSaysSoOrTheSiteIsANodeProject(t *testing.T) {
	plain := t.TempDir()
	if !Needs(plain, []string{"node", "server.js"}) || !Needs(plain, []string{"sh", "-c", "npm ci && npm run build"}) {
		t.Fatal("node commands not seen")
	}
	if Needs(plain, []string{"./server"}) || Needs(plain, []string{"sh", "-c", "echo nodejs-free"}) {
		t.Fatal("a command without node was taken for one")
	}
	pkg := t.TempDir()
	os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{}`), 0o644)
	if !Needs(pkg, []string{"./start.sh"}) {
		t.Fatal("a node project's own script was not seen")
	}
}

// fakeNVM pretends to be nvm: versions in avail install, others fail.
func fakeNVM(t *testing.T, m *Manager, avail ...string) *[]string {
	var asked []string
	m.FetchNVM = func(_ context.Context, dir string) error {
		os.MkdirAll(dir, 0o755)
		return os.WriteFile(filepath.Join(dir, "nvm.sh"), []byte("# fake"), 0o644)
	}
	m.Run = func(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
		script := args[len(args)-1]
		v := strings.Fields(strings.SplitN(script, "nvm install ", 2)[1])[0]
		asked = append(asked, v)
		for _, a := range avail {
			if a == v {
				bin := filepath.Join(m.NVMDir(), "versions", "node", "v"+strings.TrimPrefix(a, "--")+".0.0", "bin")
				os.MkdirAll(bin, 0o755)
				os.WriteFile(filepath.Join(bin, "node"), []byte("#!/bin/sh\n"), 0o755)
				return []byte("Downloading…\n" + filepath.Join(bin, "node") + "\n"), nil
			}
		}
		return nil, errors.New("N/A: version not found")
	}
	return &asked
}

func TestAMachineWithoutNodeGetsTheVersionTheSiteWants(t *testing.T) {
	m := &Manager{Root: t.TempDir(), LookPath: func(string) (string, error) { return "", errors.New("no") }}
	asked := fakeNVM(t, m, "18")
	site := t.TempDir()
	os.WriteFile(filepath.Join(site, ".nvmrc"), []byte("18"), 0o644)
	dir := m.For(context.Background(), site)
	if !strings.HasSuffix(dir, filepath.Join("v18.0.0", "bin")) {
		t.Fatalf("dir %q", dir)
	}
	// Asked once: the second site of the same version is served from the
	// cache.
	m.For(context.Background(), site)
	if len(*asked) != 1 {
		t.Fatalf("asked %v", *asked)
	}
}

func TestAVersionThatCannotBeHadNeverStopsTheSite(t *testing.T) {
	var notes []string
	m := &Manager{Root: t.TempDir(), LookPath: func(string) (string, error) { return "", errors.New("no") }, Note: func(_, msg string) { notes = append(notes, msg) }}
	fakeNVM(t, m, "--lts")
	site := t.TempDir()
	os.WriteFile(filepath.Join(site, ".nvmrc"), []byte("9"), 0o644)
	dir := m.For(context.Background(), site)
	if dir == "" || !strings.Contains(strings.Join(notes, " "), "LTS instead") {
		t.Fatalf("dir %q notes %v", dir, notes)
	}
	// With nvm failing altogether, the house's own Node.
	m2 := &Manager{Root: t.TempDir(), LookPath: func(string) (string, error) { return "", errors.New("no") }}
	m2.FetchNVM = func(context.Context, string) error { return errors.New("no network") }
	house := filepath.Join(m2.Root, "modules", "node-runtime", "bin")
	os.MkdirAll(house, 0o755)
	os.WriteFile(filepath.Join(house, "node"), []byte("#!/bin/sh\n"), 0o755)
	if got := m2.For(context.Background(), site); got != house {
		t.Fatalf("fallback %q", got)
	}
}

func TestTheMachinesNodeIsUsedWhenItIsTheOneAskedFor(t *testing.T) {
	m := &Manager{Root: t.TempDir(), LookPath: func(string) (string, error) { return "/usr/bin/node", nil }}
	m.Run = func(_ context.Context, _ []string, name string, args ...string) ([]byte, error) {
		if name == "/usr/bin/node" {
			return []byte("v22.4.1\n"), nil
		}
		t.Fatalf("nvm was run: %s %v", name, args)
		return nil, nil
	}
	site := t.TempDir()
	os.WriteFile(filepath.Join(site, "package.json"), []byte(`{"engines":{"node":">=22"}}`), 0o644)
	if got := m.For(context.Background(), site); got != "" {
		t.Fatalf("got %q", got)
	}
	plain := t.TempDir()
	if got := m.For(context.Background(), plain); got != "" {
		t.Fatalf("no version asked: %q", got)
	}
}

// With NVM=1, the real nvm is cloned and a real Node installed: a version
// this machine does not have.
func TestTheRealNVMInstallsANodeOnTheFly(t *testing.T) {
	if os.Getenv("NVM") != "1" {
		t.Skip("NVM=1 runs it (downloads nvm and a Node)")
	}
	m := &Manager{Root: t.TempDir(), LookPath: func(n string) (string, error) {
		if n == "node" {
			return "", errors.New("pretend there is none")
		}
		return exec.LookPath(n)
	}, Note: func(_, msg string) { t.Log(msg) }}
	site := t.TempDir()
	os.WriteFile(filepath.Join(site, ".nvmrc"), []byte("20"), 0o644)
	dir := m.For(context.Background(), site)
	if dir == "" {
		t.Fatal("no node")
	}
	out, err := m.run(context.Background(), os.Environ(), filepath.Join(dir, "node"), "--version")
	if err != nil || !strings.HasPrefix(string(out), "v20.") {
		t.Fatalf("%s %v", out, err)
	}
}
