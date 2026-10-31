package api

import (
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// A service's names are a delegate's to change no more than aliases are:
// a name written there would be published under the operator's DNS, and
// could be another site's.
func TestADelegateCannotChangeTheNamesOrTheContainerOfAService(t *testing.T) {
	parse := func(s string) *manifest.Manifest {
		t.Helper()
		m, err := manifest.Parse([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	cur := parse(`{"version":2,"services":{"web":{"run":{"command":["x"]},"domains":["a.test"]}}}`)
	cases := map[string]string{
		"services.web.domains": `{"version":2,"services":{"web":{"run":{"command":["x"]},"domains":["a.test","b.test"]}}}`,
		"services.web.run.":    `{"version":2,"services":{"web":{"run":{"runtime":"docker","docker":{"image":"a:1"}},"domains":["a.test"]}}}`,
		"services.api.domains": `{"version":2,"services":{"web":{"run":{"command":["x"]},"domains":["a.test"]},"api":{"run":{"command":["y"]},"domains":["a.test/api"]}}}`,
	}
	for want, next := range cases {
		// The first locked key found; a container changes two at once.
		if got := lockedChange(cur, parse(next)); !strings.HasPrefix(got, want) {
			t.Errorf("%s: lockedChange = %q", want, got)
		}
	}
	// What the service runs as a process is code, the delegate's.
	same := parse(`{"version":2,"services":{"web":{"run":{"command":["node","new.js"]},"domains":["a.test"]},"worker":{"run":{"command":["z"]}}}}`)
	if got := lockedChange(cur, same); got != "" {
		t.Errorf("a new command and a process service with no names are the delegate's; refused on %q", got)
	}
}
