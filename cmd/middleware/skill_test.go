package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/dcimorra/the-middleware/internal/chat"
)

func TestSkillInstallWritesTheEmbeddedSkillUnderTheRootOrWhereTold(t *testing.T) {
	root := t.TempDir()
	if err := skillCmd([]string{"install", "--root", root}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, ".claude", "skills", "middleware", "SKILL.md"))
	if err != nil || !bytes.Equal(got, chat.SkillContent()) {
		t.Fatalf("skill under the root: err=%v\n%s", err, got)
	}
	other := filepath.Join(t.TempDir(), "elsewhere")
	if err := skillCmd([]string{"install", other}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if err := skillCmd([]string{"remove"}); err == nil {
		t.Fatal("an unknown verb was accepted")
	}
}
