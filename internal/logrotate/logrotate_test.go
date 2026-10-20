package logrotate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAProcessKeepsWritingIntoTheSameFile holds the file open with
// O_APPEND the way a supervised process does, rotates under it, and checks
// its next write lands in the fresh log, not in the copy.
func TestAProcessKeepsWritingIntoTheSameFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "site.log")
	w, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for round := 1; round <= 5; round++ {
		w.WriteString(strings.Repeat("x", 200) + "\n")
		did, err := Rotate(path, 100, 3)
		if err != nil || !did {
			t.Fatalf("round %d: %v %v", round, did, err)
		}
		w.WriteString("after rotation\n")
		cur, _ := os.ReadFile(path)
		if string(cur) != "after rotation\n" {
			t.Fatalf("round %d: the live log holds %q", round, cur)
		}
	}
	old, _ := os.ReadFile(path + ".1")
	if !strings.Contains(string(old), "xxx") {
		t.Fatalf(".1 holds %q", old)
	}
	if _, err := os.Stat(path + ".4"); err == nil {
		t.Fatal("more than three old logs kept")
	}
	if did, _ := Rotate(path, 1<<20, 3); did {
		t.Fatal("a small log was rotated")
	}
}
