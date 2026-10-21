package release

import "testing"

func TestReleasesCompareAsSemver(t *testing.T) {
	for _, c := range []struct {
		a, b  string
		newer bool
	}{
		{"v2.0.0-alpha.10", "v2.0.0-alpha.9", true},
		{"v2.0.0-alpha.9", "v2.0.0-alpha.10", false},
		{"v2.0.0", "v2.0.0-alpha.9", true},
		{"v2.0.0-alpha.9", "v2.0.0", false},
		{"v2.0.0-beta.1", "v2.0.0-alpha.9", true},
		{"v2.1.0", "v2.0.9", true},
		{"v2.0.0", "v2.0.0", false},
		{"v2.0.0", "dev", true},
		{"dev", "v2.0.0", false},
		{"v2.0.0; rm -rf /", "v1.0.0", false},
	} {
		if got := Newer(c.a, c.b); got != c.newer {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
	if Valid("v2.0.0/../../x") || !Valid("v2.0.0-alpha.5") {
		t.Fatal("Valid")
	}
}
