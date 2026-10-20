package docs

import "testing"

func TestEveryDocumentIsServedAndNothingElse(t *testing.T) {
	list := List()
	names := map[string]bool{}
	for _, d := range list {
		names[d.Name] = true
		if _, ok := Read(d.Name); !ok || d.Title == "" {
			t.Errorf("%s: unreadable or untitled", d.Name)
		}
	}
	for _, want := range []string{"api", "manifest", "security"} {
		if !names[want] {
			t.Errorf("%s.md is not embedded", want)
		}
	}
	for _, bad := range []string{"../go", "api.md", "", "docs.go"} {
		if _, ok := Read(bad); ok {
			t.Errorf("%q was served", bad)
		}
	}
}
