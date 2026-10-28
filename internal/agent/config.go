package agent

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// A turn's HOME is data/homes/<domain>, and that folder is bind-mounted
// into a docker site's container, read and write: it is the site's. The
// CLIs kept their own state there (Claude Code its settings, hooks,
// plugins and sessions under ~/.claude, Codex its config.toml), so a
// container, or a push to the repository it deploys from, could plant a
// hook that the next turn ran on the host, outside the container. The
// CLI's own state lives instead in data/agents/config/<domain>/<key>,
// which no container mounts and the jail denies to the agent's tools:
// read by the CLI, written by the CLI and the house, and by nobody else.

// configBase is the folder under the root for every site's CLI state.
func configBase(root, home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(root, "data", "agents", "config", filepath.Base(home))
}

// ConfigHome is the folder for the CLI key's own state in this jail, made
// on first use; "" when the jail has none (a run outside a conversation).
// When it is made, what an earlier release left for that CLI under HOME
// (the entries carry names, under from) is copied in once, so a session
// resumes where it was and a login made in the site's HOME still signs
// the turn in. Copied, never linked, and only plain files and folders:
// HOME is the site's to write, and a link there could point anywhere the
// house can read.
func (j Jail) ConfigHome(key, from string, carry ...string) (string, error) {
	if j.Config == "" {
		return "", nil
	}
	dir := filepath.Join(j.Config, key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// A mark, not the folder's existence, says the carry was done: the
	// chat writes the skill into the folder before the adapter runs.
	mark := filepath.Join(dir, ".carried")
	if _, err := os.Lstat(mark); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return dir, nil
	}
	if from != "" && len(carry) > 0 {
		// What cannot be carried is lost, not fatal: the turn starts a new
		// session, as one would after a reset.
		_ = carryOver(dir, from, carry)
	}
	if f, err := os.OpenFile(mark, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
		f.Close()
	}
	return dir, nil
}

// ConfigPath is where ConfigHome puts the CLI key's folder, without
// making it.
func (j Jail) ConfigPath(key string) string {
	if j.Config == "" {
		return ""
	}
	return filepath.Join(j.Config, key)
}

// carryOver copies the carry entries of from into dst through an os.Root
// on from, so no link inside it leads out; a from that is itself a link
// is not followed at all.
func carryOver(dst, from string, carry []string) error {
	if st, err := os.Lstat(from); err != nil || !st.IsDir() {
		return err
	}
	root, err := os.OpenRoot(from)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, entry := range carry {
		err := fs.WalkDir(root.FS(), entry, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			target := filepath.Join(dst, filepath.FromSlash(p))
			switch {
			case d.IsDir():
				return os.MkdirAll(target, 0o700)
			case d.Type().IsRegular():
				return copyFromRoot(root, p, target)
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func copyFromRoot(root *os.Root, name, target string) error {
	in, err := root.Open(name)
	if err != nil {
		return err
	}
	defer in.Close()
	if st, err := in.Stat(); err != nil || !st.Mode().IsRegular() {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
