package chat

import (
	"bytes"
	"embed"
	"os"
	"path/filepath"

	"github.com/dcimorra/middleware-public/internal/fsx"
)

// The skill is embedded so a conversation always runs with the one this
// binary was built with: a skill file left on disk by an older release
// would describe a manifest field or a command that no longer exists.
//
//go:embed skills
var skillFS embed.FS

// skillPath is the skill inside the embedded tree, and SkillName the
// folder it is installed under: Claude Code loads
// ~/.claude/skills/<name>/SKILL.md by that name.
const (
	skillPath = "skills/middleware/SKILL.md"
	SkillName = "middleware"
)

// SkillContent returns the embedded skill.
func SkillContent() []byte {
	data, err := skillFS.ReadFile(skillPath)
	if err != nil {
		// The file is embedded at build time; a missing one is a build
		// that cannot exist, not a condition to handle at runtime.
		panic("chat: embedded skill missing: " + err.Error())
	}
	return data
}

// InstallSkill writes the skill as dir/SKILL.md, atomically and only when
// the content differs, so a turn every few seconds does not rewrite an
// unchanged file. dir is the skill's own folder (…/skills/middleware).
func InstallSkill(dir string) error {
	return writeIfChanged(filepath.Join(dir, "SKILL.md"), SkillContent())
}

// installSkillFor puts the skill where the provider's CLI will read it
// from the run's HOME. Claude Code loads skills from ~/.claude/skills;
// the Antigravity CLI from ~/.gemini/config/skills, so the same file goes
// there too.
func installSkillFor(home, provider string) error {
	if home == "" {
		return nil
	}
	if err := InstallSkill(filepath.Join(home, ".claude", "skills", SkillName)); err != nil {
		return err
	}
	if provider == "agy" {
		return InstallSkill(filepath.Join(home, ".gemini", "config", "skills", SkillName))
	}
	return nil
}

func writeIfChanged(path string, data []byte) error {
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) {
		return nil
	}
	return fsx.WriteAtomic(path, data, 0o644)
}

// needsSkillHint are the agents that do not load Claude skills by
// themselves: OpenCode and the vendors' own CLIs, and Antigravity, which
// loads its own skills but only when it thinks one applies. DeepSeek and Z.AI
// run Claude Code, which does.
var needsSkillHint = map[string]bool{"agy": true, "opencode": true, "openai": true, "kimi": true, "qwen": true, "mistral": true, "xai": true}

// SkillHint is what a provider that does not load Claude skills is told
// at the top of its system context, so it reads the file anyway.
const SkillHint = "Read ~/.claude/skills/middleware/SKILL.md first: it says how sites, workers and tasks are built, published and checked on this machine. It is outside the site's folder and reading it is allowed: it is the house's guide, the one file outside the folder you may always read."

// projectHint tells an agent that does not read CLAUDE.md to read the
// project's, when the project has one and no AGENTS.md: its rules (why
// this runtime, what must not be changed) were written for Claude and
// apply to whoever works on it.
func projectHint(workdir string) string {
	if workdir == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(workdir, "AGENTS.md")); err == nil {
		return ""
	}
	if _, err := os.Stat(filepath.Join(workdir, "CLAUDE.md")); err != nil {
		return ""
	}
	return "This project's instructions are in CLAUDE.md at the root of its folder. They were written for Claude Code and apply to you as well: read that file before changing anything."
}
