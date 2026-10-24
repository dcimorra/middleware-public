package helper

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The requirements run is this binary's own install --requirements, for the
// user and root the installed service names: nothing of it comes from the
// caller but whether to add the browser.
func TestTheRequirementsRunIsTheHousesOwnInstallReadFromItsUnit(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "middleware")
	os.WriteFile(bin, []byte("binary"), 0o755)
	unit := func(user, root, exec string) {
		os.WriteFile(filepath.Join(dir, "middleware.service"), []byte("[Service]\nUser="+user+"\nEnvironment=MIDDLEWARE_ROOT="+root+"\nExecStart="+exec+" serve --root "+root+"\n"), 0o644)
	}
	rec := &Recorder{Outputs: map[string]string{}, Fail: map[string]error{}}
	s := &Server{Exec: rec, SystemdDir: dir, Binary: bin}
	call := func(args string) Response {
		return s.Handle(context.Background(), Peer{UID: 1000}, Request{Op: OpInstallRun, Args: json.RawMessage(args)})
	}
	unit("ubuntu", "/srv/middleware", bin)
	if r := call(`{"browser":true}`); !r.OK {
		t.Fatalf("%+v", r)
	}
	last, _ := rec.Last()
	line := last.Line()
	for _, want := range []string{"systemd-run", "--unit=middleware-requirements", "--collect", "--no-block", "StandardOutput=truncate:/srv/middleware/data/requirements.log", bin + " install --requirements --yes --panel --root /srv/middleware --user ubuntu --binary " + bin, "--browser yes"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in %s", want, line)
		}
	}

	// One at a time.
	rec.Outputs["systemctl is-active middleware-requirements"] = "active\n"
	if r := call(`{}`); r.OK || !strings.Contains(r.Error, "already") {
		t.Fatalf("a second run while one is active: %+v", r)
	}
	delete(rec.Outputs, "systemctl is-active middleware-requirements")

	// What the caller cannot choose, and what a tampered unit cannot make it run.
	for _, c := range []struct{ args, user, root, exec, want string }{
		{`{"command":"rm -rf /"}`, "ubuntu", "/srv/middleware", bin, "unknown field"},
		{`{}`, "root", "/srv/middleware", bin, "not an operator's user"},
		{`{}`, "ubuntu", "srv", bin, "absolute"},
		{`{}`, "ubuntu", "/srv/middleware", "/tmp/other", "the helper is"},
	} {
		unit(c.user, c.root, c.exec)
		n := len(rec.Calls)
		r := call(c.args)
		if r.OK || !strings.Contains(r.Error, c.want) {
			t.Errorf("%+v: %+v, want %q", c, r, c.want)
		}
		for _, l := range rec.Lines()[n:] {
			if strings.HasPrefix(l, "systemd-run") {
				t.Errorf("%+v ran %s", c, l)
			}
		}
	}
}
