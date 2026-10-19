package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAtomicLeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.json")
	if err := WriteAtomic(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(path, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected one file, got %d", len(entries))
	}
	got, _ := os.ReadFile(path)
	if string(got) != "two" {
		t.Fatalf("got %q", got)
	}
}

func TestReadTrimmedDropsTheTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pointer")
	os.WriteFile(path, []byte("  .next-2026\n"), 0o644)
	got, err := ReadTrimmed(path)
	if err != nil || got != ".next-2026" {
		t.Fatalf("got %q, %v", got, err)
	}
}
