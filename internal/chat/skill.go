package chat

import (
	"bytes"
	"embed"
	"os"
	"path"
	"path/filepath"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/fsx"
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
// there too. Claude Code now runs with its own folder outside HOME
// (agent.Jail.ConfigHome) and loads its skills from there; the copy in
// HOME stays, as the guide every other agent is told to read.
func installSkillFor(j agent.Jail, provider string) error {
	home := j.Home
	if home == "" {
		return nil
	}
	if err := installSkillUnder(home, path.Join(".claude", "skills", SkillName)); err != nil {
		return err
	}
	if cfg := j.ConfigPath(provider); cfg != "" && !needsSkillHint[provider] {
		if err := InstallSkill(filepath.Join(cfg, "skills", SkillName)); err != nil {
			return err
		}
	}
	if provider == "agy" {
		return installSkillUnder(home, path.Join(".gemini", "config", "skills", SkillName))
	}
	return nil
}

// installSkillUnder is InstallSkill into rel inside home, through an
// os.Root: the HOME is the site's to write, and a .claude made a link to
// another site's folder had the house write the skill there.
func installSkillUnder(home, rel string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return err
	}
	defer root.Close()
	data := SkillContent()
	file := path.Join(rel, "SKILL.md")
	if cur, err := root.ReadFile(file); err == nil && bytes.Equal(cur, data) {
		return nil
	}
	if err := root.MkdirAll(rel, 0o755); err != nil {
		return err
	}
	tmp := path.Join(rel, ".SKILL.md."+newID()+".tmp")
	if err := root.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := root.Rename(tmp, file); err != nil {
		_ = root.Remove(tmp)
		return err
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
