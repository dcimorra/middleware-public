package relocate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The plan says, before anything moves, what is copied (without what is
// installed again), where dependencies are installed and with what tool,
// whichever folders they are in, and what will not travel: no folder name
// is assumed.
func TestThePlanSaysWhatAMoveWillDoWhateverTheFoldersAreCalled(t *testing.T) {
	root := t.TempDir()
	site := filepath.Join(root, "sites", "a.test")
	write := func(rel, body string) {
		p := filepath.Join(site, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	write("site.json", `{"version":2,"node":"22","run":{"command":["node","web/server.js"]}}`)
	write("web/package.json", `{}`)
	write("web/package-lock.json", `{}`)
	write("web/node_modules/big/index.js", strings.Repeat("x", 100000))
	write("cliente/app/package.json", `{}`)
	write("cliente/app/node_modules/x/index.js", "x")
	write("api/.venv/pyvenv.cfg", "home = /usr/bin")
	os.Symlink("/usr/bin/python3", filepath.Join(site, "api", ".venv", "python"))
	write("web/server.js", "hello")

	a := Analyze(site, MoveParts(root, "a.test"), true, []string{"a.test"})
	var dirs []string
	for _, in := range a.Installs {
		dirs = append(dirs, in.Dir+":"+in.Command[1]+":"+map[bool]string{true: "lock", false: "nolock"}[in.Lockfile])
	}
	if got := strings.Join(dirs, ","); got != "cliente/app:install:nolock,web:ci:lock" {
		t.Fatalf("installs: %s", got)
	}
	if a.Bytes >= 100000 {
		t.Fatalf("what is installed again was counted as copied: %d bytes", a.Bytes)
	}
	if a.Node != "22" || len(a.Links) != 1 || a.Links[0].Path != "api/.venv/python" {
		t.Fatalf("node %q links %+v", a.Node, a.Links)
	}
	w := strings.Join(a.Warnings, " | ")
	if !strings.Contains(w, "cliente/app has no lockfile") || !strings.Contains(w, "1 links point out") {
		t.Fatalf("warnings: %s", w)
	}
}
