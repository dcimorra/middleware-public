package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestPickAgentModel(t *testing.T) {
	limits := map[string][]string{"claude": {"opus", "sonnet"}, "codex": {}}
	known := []string{"haiku", "sonnet", "opus"}
	for _, c := range []struct {
		limits          map[string][]string
		provider, model string
		want            string
		refused         bool
	}{
		{nil, "anything", "x", "x", false},
		{limits, "claude", "opus", "opus", false},
		{limits, "claude", "haiku", "", true},
		// The default is the first allowed one the provider knows, in the
		// provider's order, not the limit's.
		{limits, "claude", "", "sonnet", false},
		{limits, "codex", "gpt-9", "gpt-9", false},
		{limits, "codex", "", "", false},
		{limits, "kimi", "", "", true},
		{map[string][]string{}, "claude", "", "", true},
		// Nothing allowed is known here yet: the limit's own first.
		{map[string][]string{"claude": {"future"}}, "claude", "", "future", false},
	} {
		got, err := PickAgentModel(c.limits, c.provider, c.model, known)
		if c.refused {
			if !errors.Is(err, ErrAgentNotAllowed) {
				t.Errorf("%v %s/%s: %q %v, want refused", c.limits, c.provider, c.model, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%v %s/%s: %q %v, want %q", c.limits, c.provider, c.model, got, err, c.want)
		}
	}
}

// What is kept is normalized, and an empty map stays empty: no agent at
// all is not every agent.
func TestCheckAgents(t *testing.T) {
	got, err := CheckAgents(map[string][]string{" Claude ": {"b", " a", "b", ""}, "codex": nil})
	if err != nil || strings.Join(got["claude"], ",") != "a,b" || got["codex"] == nil || len(got["codex"]) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := CheckAgents(map[string][]string{}); err != nil || got == nil {
		t.Fatalf("empty: %v %v", got, err)
	}
	if got, _ := CheckAgents(nil); got != nil {
		t.Fatalf("nil: %v", got)
	}
	for _, bad := range []map[string][]string{{"no/slash": {}}, {"claude": {"two words"}}} {
		if _, err := CheckAgents(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
