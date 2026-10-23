package chat

import (
	"strings"
	"testing"
)

func TestPermissionsHaveOneNameAndTheOldLevelsKeepTheirs(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{nil, "none"},
		{[]string{"read"}, "read"},
		{[]string{"web", "read"}, "web"},
		{[]string{"shell", "web", "edit", "read"}, "write"},
		// "web" alone is not the old "web", which read the folder too.
		{[]string{"web"}, "perms:web"},
		{[]string{"read", "shell"}, "perms:read,shell"},
	} {
		got, err := PermsName(c.in)
		if err != nil || got != c.want {
			t.Errorf("PermsName(%v) = %q %v, want %q", c.in, got, err, c.want)
		}
		if back := strings.Join(PermsOf(got), ","); back != strings.Join(mustSorted(c.in), ",") {
			t.Errorf("PermsOf(%q) = %q", got, back)
		}
	}
	if _, err := PermsName([]string{"read", "sudo"}); err == nil {
		t.Fatal("an unknown permission was accepted")
	}
}

func mustSorted(in []string) []string {
	has := map[string]bool{}
	for _, p := range in {
		has[p] = true
	}
	out := []string{}
	for _, p := range Permissions {
		if has[p] {
			out = append(out, p)
		}
	}
	return out
}

func TestALevelOfPermissionsListsOnlyTheirToolsAndNeverSubagents(t *testing.T) {
	l := Level("perms:read,shell")
	got := strings.Join(l.Tools, " ")
	if got != "Read Glob Grep Bash BashOutput KillShell" || !l.Writes || l.Disallowed[0] != "Task" {
		t.Fatalf("%+v", l)
	}
	if l := Level("perms:web"); strings.Join(l.Tools, " ") != "WebSearch WebFetch" || l.Writes {
		t.Fatalf("web alone: %+v", l)
	}
	if l := Level("perms:read,edit,web,shell"); l.Tools != nil || l.Name != "write" {
		t.Fatalf("everything is the panel's jail: %+v", l)
	}
	// A word nobody knows closes everything rather than opening anything.
	if l := Level("perms:read,root"); l.Name != "none" {
		t.Fatalf("%+v", l)
	}
	sys := SiteSystem(Conversation{Domain: "a.test", Tools: "perms:read,shell"})
	if !strings.Contains(sys, "run commands") || !strings.Contains(sys, "you cannot change files") {
		t.Fatalf("%s", sys)
	}
}
