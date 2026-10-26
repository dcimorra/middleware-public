package main

import (
	"testing"

	"github.com/dcimorra/middleware-public/internal/manifest"
)

// A site that serves its whole folder would publish openspec/ (every
// proposal, every design) to anyone who asked for it.
func TestASiteThatServesItsFolderIsNotSetUpForOpenSpec(t *testing.T) {
	cases := map[string]bool{
		`{"version":2}`:                                   true,
		`{"version":2,"static":{"root":"."}}`:             true,
		`{"version":2,"static":{"root":"public"}}`:        false,
		`{"version":2,"run":{"command":["node","s.js"]}}`: false,
	}
	for src, want := range cases {
		m, err := manifest.Parse([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if got := servesItsFolder(m); got != want {
			t.Errorf("%s: %v, want %v", src, got, want)
		}
	}
}
