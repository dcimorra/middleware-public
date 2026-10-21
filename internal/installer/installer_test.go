package installer

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testParams(t *testing.T) (Params, Paths) {
	t.Helper()
	dir := t.TempDir()
	p := DefaultParams()
	p.User = "alice"
	p.Root = filepath.Join(dir, "root")
	p.Binary = filepath.Join(dir, "bin", "middleware")
	p.RunDir = filepath.Join(dir, "run")
	p.HelperSocket = filepath.Join(dir, "run", "helper.sock")
	p.Version = "1.2.3"
	paths := Paths{
		SystemdDir:  filepath.Join(dir, "etc", "systemd", "system"),
		TmpfilesDir: filepath.Join(dir, "etc", "tmpfiles.d"),
		SysctlDir:   filepath.Join(dir, "etc", "sysctl.d"),
	}
	return p, paths
}

func testInstaller(t *testing.T) (*Installer, *Recorder, *bytes.Buffer) {
	t.Helper()
	p, paths := testParams(t)
	rec := NewRecorder()
	rec.Path["apt-get"] = true
	rec.Path["git"] = true
	rec.Path["wg"] = true
	// A fake binary stands in for the running executable.
	src := filepath.Join(t.TempDir(), "middleware-src")
	if err := os.WriteFile(src, []byte("#!/bin/sh\necho middleware 1.2.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	in := &Installer{
		Exec:           rec,
		Params:         p,
		Paths:          paths,
		Out:            &out,
		Source:         src,
		OperatorExists: func(string) bool { return true },
		FindBrowser:    func(string) string { return "" },
	}
	return in, rec, &out
}

func TestRenderedUnitsCarryTheUserTheRootAndKillModeProcess(t *testing.T) {
	p := DefaultParams()
	p.User = "alice"
	p.Root = "/srv/mw"
	p.Version = "9.9.9"
	files, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	svc := files[ServiceUnit]
	for _, want := range []string{
		"User=alice",
		"WorkingDirectory=/srv/mw",
		"ExecStart=/usr/local/bin/middleware serve --root /srv/mw",
		"KillMode=process",
		"Restart=always",
		"AmbientCapabilities=CAP_NET_BIND_SERVICE",
		"LimitNOFILE=65536",
		"NoNewPrivileges=no",
		"Requires=middleware.socket",
		"middleware 9.9.9 install",
	} {
		if !strings.Contains(svc, want) {
			t.Errorf("service unit lacks %q:\n%s", want, svc)
		}
	}
	sock := files[SocketUnit]
	for _, want := range []string{"ListenStream=127.0.0.1:46100", "ReuseAddr=yes"} {
		if !strings.Contains(sock, want) {
			t.Errorf("socket unit lacks %q", want)
		}
	}
	helper := files[HelperUnit]
	for _, want := range []string{
		"ExecStart=/usr/local/bin/middleware helper --socket /run/middleware/helper.sock --allow-gid middleware",
		"ProtectSystem=strict",
		"ReadWritePaths=/etc/wireguard /etc/sysctl.d /run/middleware",
		"CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_SYS_ADMIN",
		"User=root",
	} {
		if !strings.Contains(helper, want) {
			t.Errorf("helper unit lacks %q", want)
		}
	}
	if !strings.Contains(files[TmpfilesIn], "d /run/middleware 0750 root middleware -") {
		t.Errorf("tmpfiles entry wrong:\n%s", files[TmpfilesIn])
	}
	if !strings.Contains(files[SysctlIn], "net.ipv4.ip_local_reserved_ports = 33000-47000") {
		t.Errorf("sysctl entry wrong:\n%s", files[SysctlIn])
	}
}

func TestWithoutTheHelperNoHelperUnitIsRendered(t *testing.T) {
	p := DefaultParams()
	p.User = "alice"
	p.Helper = false
	files, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[HelperUnit]; ok {
		t.Fatal("helper unit rendered with Helper=false")
	}
}

func TestRenderRefusesParamsThatWouldNotRun(t *testing.T) {
	cases := map[string]func(*Params){
		"no user":        func(p *Params) { p.User = "" },
		"relative root":  func(p *Params) { p.Root = "srv/mw" },
		"inverted ports": func(p *Params) { p.PortsFrom, p.PortsTo = 47000, 33000 },
		"empty listen":   func(p *Params) { p.Listen = "" },
	}
	for name, mutate := range cases {
		p := DefaultParams()
		p.User = "alice"
		mutate(&p)
		if _, err := Render(p); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestAFreshInstallRunsTheStepsInOrderAndWritesEveryFile(t *testing.T) {
	in, rec, out := testInstaller(t)
	rep, err := in.Install()
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	want := []string{
		"sudo apt-get install -y -q curl",
		"sudo groupadd --system middleware",
		"sudo usermod -aG middleware alice",
		"write " + in.Params.Binary,
		"write " + filepath.Join(in.Paths.SystemdDir, ServiceUnit),
		"write " + filepath.Join(in.Paths.SystemdDir, SocketUnit),
		"write " + filepath.Join(in.Paths.SystemdDir, HelperUnit),
		"write " + filepath.Join(in.Paths.TmpfilesDir, TmpfilesIn),
		"write " + filepath.Join(in.Paths.SysctlDir, SysctlIn),
		"sudo sysctl -q -p " + filepath.Join(in.Paths.SysctlDir, SysctlIn),
		"sudo systemd-tmpfiles --create " + filepath.Join(in.Paths.TmpfilesDir, TmpfilesIn),
		"sudo systemctl daemon-reload",
		"sudo systemctl enable middleware.socket",
		"sudo systemctl enable middleware-helper.service",
		"sudo systemctl enable middleware.service",
		"sudo systemctl start middleware.socket",
		"sudo systemctl start middleware-helper.service",
		"sudo systemctl start middleware.service",
	}
	got := rec.Mutations()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("command sequence differs.\nwant:\n%s\n\ngot:\n%s\n\noutput:\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"), out)
	}
	for _, d := range rootSubdirs {
		if st, err := os.Stat(filepath.Join(in.Params.Root, d)); err != nil || !st.IsDir() {
			t.Errorf("root lacks %s/", d)
		}
	}
	if b, err := os.ReadFile(in.Params.Binary); err != nil || !strings.Contains(string(b), "middleware 1.2.3") {
		t.Errorf("binary not installed: %v", err)
	}
	if len(rep.Changed) < 8 {
		t.Errorf("expected most steps to report a change, got %v", rep.Changed)
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if !strings.HasPrefix(line, "✓ ") {
			t.Errorf("line without a mark: %q", line)
		}
	}
}

func TestASecondInstallChangesNothing(t *testing.T) {
	in, rec, out := testInstaller(t)
	if _, err := in.Install(); err != nil {
		t.Fatalf("first install: %v\n%s", err, out)
	}
	// The run dir is created by systemd-tmpfiles on the real machine; the
	// recorder does not run it, so stand in for it.
	if err := os.MkdirAll(in.Params.RunDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// sysctl answers with the reserved range after the first apply.
	rec.Set("sysctl -n net.ipv4.ip_local_reserved_ports", "33000-47000", nil)
	rec.Calls = nil
	out.Reset()
	rep, err := in.Install()
	if err != nil {
		t.Fatalf("second install: %v\n%s", err, out)
	}
	if m := rec.Mutations(); len(m) != 0 {
		t.Fatalf("second run changed the machine:\n%s\n\noutput:\n%s", strings.Join(m, "\n"), out)
	}
	if len(rep.Changed) != 0 {
		t.Fatalf("second run reported changes: %v", rep.Changed)
	}
	if !strings.Contains(out.String(), "unchanged") && !strings.Contains(out.String(), "current") {
		t.Fatalf("second run output does not say things were current:\n%s", out)
	}
}

func TestANewBinaryRestartsTheServiceButNotTheSocket(t *testing.T) {
	in, rec, out := testInstaller(t)
	if _, err := in.Install(); err != nil {
		t.Fatalf("first install: %v\n%s", err, out)
	}
	if err := os.MkdirAll(in.Params.RunDir, 0o750); err != nil {
		t.Fatal(err)
	}
	rec.Set("sysctl -n net.ipv4.ip_local_reserved_ports", "33000-47000", nil)
	if err := os.WriteFile(in.Source, []byte("#!/bin/sh\necho middleware 1.2.4\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec.Calls = nil
	if _, err := in.Install(); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	got := strings.Join(rec.Mutations(), "\n")
	want := strings.Join([]string{
		"write " + in.Params.Binary,
		"sudo systemctl restart middleware-helper.service",
		"sudo systemctl restart middleware.service",
	}, "\n")
	if got != want {
		t.Fatalf("want:\n%s\ngot:\n%s", want, got)
	}
}

func TestAFailedStepPrintsACrossAndStops(t *testing.T) {
	in, rec, out := testInstaller(t)
	rec.Fail[in.Params.Binary] = errors.New("read-only file system")
	_, err := in.Install()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(out.String(), "✗ binary") {
		t.Fatalf("no cross for the failed step:\n%s", out)
	}
	for _, c := range rec.Calls {
		if strings.HasPrefix(c, "sudo systemctl") {
			t.Fatalf("systemctl ran after a failed step: %s", c)
		}
	}
}

func TestNoStartEnablesButNeverStarts(t *testing.T) {
	in, rec, out := testInstaller(t)
	in.NoStart = true
	in.OperatorExists = func(string) bool { return false }
	if _, err := in.Install(); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	for _, c := range rec.Calls {
		if strings.Contains(c, "systemctl start") || strings.Contains(c, "systemctl restart") || strings.Contains(c, " setup ") {
			t.Fatalf("--no-start ran %q", c)
		}
	}
	if !strings.Contains(out.String(), "setup --root") {
		t.Fatalf("expected the setup hint:\n%s", out)
	}
}

func TestInstallEndsWithTheOneTimeCodeWhenNoOperatorExists(t *testing.T) {
	in, rec, out := testInstaller(t)
	in.OperatorExists = func(string) bool { return false }
	rec.Set(in.Params.Binary+" setup --root "+in.Params.Root, "one-time code: 123456", nil)
	if _, err := in.Install(); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	s := out.String()
	if !strings.Contains(s, "one-time code: 123456") {
		t.Fatalf("missing the code:\n%s", s)
	}
	if strings.Index(s, "✓ operator") > strings.Index(s, "123456") {
		t.Fatalf("the code must come after the step lines:\n%s", s)
	}
}

func TestADomainGivenAtInstallReachesSetupEvenWithAnOperator(t *testing.T) {
	in, rec, out := testInstaller(t)
	in.Params.Domain, in.Params.Door = "panel.example.com", "tunnel"
	in.OperatorExists = func(string) bool { return true }
	rec.Set(in.Params.Binary+" setup --root "+in.Params.Root+" --domain panel.example.com --door tunnel", "Open https://panel.example.com/ and sign in.", nil)
	if _, err := in.Install(); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "https://panel.example.com/") {
		t.Fatalf("setup --domain did not run:\n%s", out)
	}
}

func TestUninstallRemovesUnitsButKeepsTheRoot(t *testing.T) {
	in, rec, out := testInstaller(t)
	if _, err := in.Install(); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	marker := filepath.Join(in.Params.Root, "sites", "example.com", "site.json")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec.Calls = nil
	out.Reset()
	if _, err := in.Uninstall(); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	want := []string{
		"sudo systemctl stop middleware.socket",
		"sudo systemctl stop middleware-helper.service",
		"sudo systemctl stop middleware.service",
		"sudo systemctl disable middleware.socket",
		"sudo systemctl disable middleware-helper.service",
		"sudo systemctl disable middleware.service",
		"remove " + filepath.Join(in.Paths.SystemdDir, ServiceUnit),
		"remove " + filepath.Join(in.Paths.SystemdDir, SocketUnit),
		"remove " + filepath.Join(in.Paths.SystemdDir, HelperUnit),
		"remove " + filepath.Join(in.Paths.TmpfilesDir, TmpfilesIn),
		"remove " + filepath.Join(in.Paths.SysctlDir, SysctlIn),
		"sudo systemctl daemon-reload",
		"remove " + in.Params.Binary,
	}
	got := rec.Mutations()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("want:\n%s\ngot:\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("uninstall touched the sites: %v", err)
	}
	if !strings.Contains(out.String(), "Kept "+in.Params.Root) {
		t.Fatalf("uninstall must say the root was kept:\n%s", out)
	}
	for _, p := range in.filePaths() {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s still exists", p)
		}
	}
}

func TestDoctorPrintsOneMarkedLinePerCheck(t *testing.T) {
	in, rec, out := testInstaller(t)
	if _, err := in.Install(); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	out.Reset()

	// Real listeners: the loopback socket and the helper socket are
	// checked by connecting, not by asking systemd.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	in.Params.Listen = ln.Addr().String()
	if err := os.MkdirAll(in.Params.RunDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// A unix socket path must stay short; t.TempDir can be long.
	short := filepath.Join(os.TempDir(), "mw-doctor-test.sock")
	os.Remove(short)
	uln, err := net.Listen("unix", short)
	if err != nil {
		t.Fatal(err)
	}
	defer uln.Close()
	in.Params.HelperSocket = short

	rec.Set(in.Params.Binary+" version", "middleware 1.2.3", nil)
	rec.Set("sysctl -n net.ipv4.ip_local_reserved_ports", "33000-47000", nil)
	in.Params.User = "" // ownership is not asserted against a user that does not exist here

	checks, all := in.Doctor()
	if !all {
		t.Fatalf("expected every check to pass:\n%s", out)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != len(checks) {
		t.Fatalf("%d lines for %d checks:\n%s", len(lines), len(checks), out)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "✓ ") {
			t.Errorf("unexpected line %q", l)
		}
		if len(l) < 22 {
			t.Errorf("line too short to be aligned: %q", l)
		}
	}
	names := map[string]bool{}
	for _, c := range checks {
		names[c.Name] = true
	}
	for _, want := range []string{"binary", "middleware.service", "middleware.socket", "middleware-helper.service", "loopback socket", "helper", "root directory", "reserved ports", "docker", "wireguard", "disk"} {
		if !names[want] {
			t.Errorf("no check named %q", want)
		}
	}
}

func TestDoctorFlagsWhatIsWrongWithoutStopping(t *testing.T) {
	in, rec, out := testInstaller(t)
	// Nothing installed: the socket does not answer, the units are
	// missing, the binary is missing.
	in.Params.Listen = "127.0.0.1:1" // nothing listens on port 1
	in.Params.HelperSocket = filepath.Join(t.TempDir(), "nope.sock")
	rec.Set("sysctl -n net.ipv4.ip_local_reserved_ports", "", nil)
	checks, all := in.Doctor()
	if all {
		t.Fatal("doctor passed a machine with nothing on it")
	}
	failed := 0
	for _, c := range checks {
		if !c.OK {
			failed++
		}
	}
	if failed < 5 {
		t.Fatalf("expected several failures, got %d:\n%s", failed, out)
	}
	if !strings.Contains(out.String(), "✗ binary") || !strings.Contains(out.String(), "✗ reserved ports") {
		t.Fatalf("missing crosses:\n%s", out)
	}
}

func TestTheSystemExecRunsRealCommands(t *testing.T) {
	var s System
	out, err := s.Run("sh", "-c", "echo hello")
	if err != nil || out != "hello" {
		t.Fatalf("got %q, %v", out, err)
	}
	if _, err := s.Run("sh", "-c", "exit 3"); err == nil {
		t.Fatal("exit status not reported")
	}
	if _, err := s.LookPath("sh"); err != nil {
		t.Fatal(err)
	}
}
