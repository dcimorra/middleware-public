package relocate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// What moves left is listed with its kind and size, and only what is
// listed can be cleaned: a placed site is taken out as a failed move
// would have, and a reception in progress is not a leftover.
func TestWhatMovesLeftIsListedAndOnlyThatIsCleaned(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string, age time.Duration) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
		old := time.Now().Add(-age)
		os.Chtimes(p, old, old)
		os.Chtimes(filepath.Dir(p), old, old)
	}
	write("data/relocate/old.test/site/index.html", "abandoned", 3*time.Hour)
	old := time.Now().Add(-3 * time.Hour)
	os.Chtimes(filepath.Join(root, "data/relocate/old.test"), old, old)
	write("data/relocate/now.test/site/index.html", "arriving", 0)
	write("data/homes/x.test.before-20261024T000000Z/big", "a whole HOME", 0)
	write("data/chat/x.test.json.before-20261024T000000Z", "{}", 0)
	write("data/relocate/placed/gone.test.json", `{"domain":"gone.test","parts":["data/homes/gone.test"]}`, 0)
	write("data/homes/gone.test/file", "arrived for a site that did not come up", 0)
	write("data/relocate/placed/here.test.json", `{"domain":"here.test","parts":["data/homes/here.test"]}`, 0)
	write("sites/here.test/site.json", "{}", 0)
	write("data/homes/here.test/file", "a live site's HOME", 0)

	kinds := map[string]string{}
	for _, l := range Leftovers(root, time.Now()) {
		kinds[l.Path] = l.Kind
	}
	want := map[string]string{
		"data/relocate/old.test":                        "staging",
		"data/homes/x.test.before-20261024T000000Z":     "aside",
		"data/chat/x.test.json.before-20261024T000000Z": "aside",
		"data/relocate/placed/gone.test.json":           "placed",
		"data/relocate/placed/here.test.json":           "record",
	}
	for p, k := range want {
		if kinds[p] != k {
			t.Errorf("%s: %q, want %q (all: %v)", p, kinds[p], k, kinds)
		}
	}
	if _, ok := kinds["data/relocate/now.test"]; ok {
		t.Error("a reception in progress was listed")
	}
	for p := range want {
		if err := Clean(root, p, time.Now()); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if got := Leftovers(root, time.Now()); len(got) != 0 {
		t.Fatalf("left after cleaning: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(root, "data/homes/gone.test")); err == nil {
		t.Error("the placed site's data stayed")
	}
	if _, err := os.Stat(filepath.Join(root, "data/homes/here.test/file")); err != nil {
		t.Error("a live site's HOME went with its record")
	}
	for _, p := range []string{"sites/here.test", "../outside", "data/relocate/now.test"} {
		if err := Clean(root, p, time.Now()); err != ErrNotALeftover {
			t.Errorf("%s: %v, want it refused", p, err)
		}
	}
}
