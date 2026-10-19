package manifest

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAnUnknownFieldIsRejected(t *testing.T) {
	_, err := Parse([]byte(`{"version":2,"run":{"comand":["x"]}}`))
	if err == nil {
		t.Fatal("a typo in a field name must not parse")
	}
}

func TestDurationsReadAsStringsAndSeconds(t *testing.T) {
	m, err := Parse([]byte(`{"version":2,"run":{"command":["x"],"drain":"3s","health":{"timeout":10}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(m.Run.Drain) != 3*time.Second || time.Duration(m.Run.Health.Timeout) != 10*time.Second {
		t.Fatalf("drain=%v timeout=%v", time.Duration(m.Run.Drain), time.Duration(m.Run.Health.Timeout))
	}
}

func TestModePrecedence(t *testing.T) {
	cases := []struct {
		in   string
		want Mode
	}{
		{`{"version":2,"disabled":true,"hold":{"enabled":true},"run":{"command":["x"]}}`, ModeDisabled},
		{`{"version":2,"hold":{"enabled":true},"run":{"command":["x"]}}`, ModeHold},
		{`{"version":2,"run":{"command":["x"]},"upstream":"http://127.0.0.1:1"}`, ModeRun},
		{`{"version":2,"upstream":"http://127.0.0.1:1"}`, ModeUpstream},
		{`{"version":2}`, ModeStatic},
	}
	for _, c := range cases {
		m, err := Parse([]byte(c.in))
		if err != nil {
			t.Fatal(err)
		}
		if got := m.Mode(); got != c.want {
			t.Errorf("%s: got %s want %s", c.in, got, c.want)
		}
	}
}

func TestAWorkerMustWriteItsPort(t *testing.T) {
	_, err := Parse([]byte(`{"version":2,"workers":{"api":{"command":["x"]}}}`))
	if err == nil {
		t.Fatal("a worker without a port must not validate")
	}
}

func TestRevisionChangesWhenThePointerFileChanges(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".current"), []byte("build-1\n"), 0o644)
	m, _ := Parse([]byte(`{"version":2,"run":{"command":["./s","--port","$PORT"],"env_from_file":{"DIST":".current"}}}`))
	a, err := Resolve("a.test", dir, m)
	if err != nil {
		t.Fatal(err)
	}
	if a.Env["DIST"] != "build-1" {
		t.Fatalf("pointer not read: %q", a.Env["DIST"])
	}
	os.WriteFile(filepath.Join(dir, ".current"), []byte("build-2\n"), 0o644)
	b, _ := Resolve("a.test", dir, m)
	if a.Revision == b.Revision {
		t.Fatal("revision did not change with the pointer")
	}
	c, _ := Resolve("a.test", dir, m)
	if b.Revision != c.Revision {
		t.Fatal("revision is not stable")
	}
}

func TestThePortIsNotPartOfTheRevision(t *testing.T) {
	m, _ := Parse([]byte(`{"version":2,"run":{"command":["./s","--port","$PORT"],"env":{"URL":"http://127.0.0.1:$PORT"}}}`))
	s, _ := Resolve("a.test", "/tmp/x", m)
	if s.Command[2] != "$PORT" {
		t.Fatalf("$PORT expanded too early: %v", s.Command)
	}
	cmd, env := s.WithPort(4000)
	if cmd[2] != "4000" || env["URL"] != "http://127.0.0.1:4000" {
		t.Fatalf("cmd=%v env=%v", cmd, env)
	}
}

func TestUnknownVariablesAreLeftAlone(t *testing.T) {
	if got := Expand("$HOME/$DOMAIN", map[string]string{"DOMAIN": "a.test"}); got != "$HOME/a.test" {
		t.Fatalf("got %q", got)
	}
}

func TestEnvFileSupportsQuotesCommentsAndExport(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("# c\nexport A=1\nB=\"two words\"\nC='x'\nbad line\n"), 0o644)
	got, err := readEnvFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if got["A"] != "1" || got["B"] != "two words" || got["C"] != "x" || len(got) != 3 {
		t.Fatalf("got %v", got)
	}
}

func TestValidDomain(t *testing.T) {
	for _, ok := range []string{"a.test", "sub.example.co.uk", "x-y.example.com"} {
		if !ValidDomain(ok) {
			t.Errorf("%s should be valid", ok)
		}
	}
	for _, bad := range []string{"Example.com", "nodot", "-a.test", "a..test", "a.test/"} {
		if ValidDomain(bad) {
			t.Errorf("%s should be invalid", bad)
		}
	}
}
