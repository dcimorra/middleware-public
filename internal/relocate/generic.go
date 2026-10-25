package relocate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dcimorra/middleware-public/internal/archive"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/migrate"
)

// Importing an app from any server, not only a V1 house: a folder
// anywhere, run however it is run there (pm2, systemd, a screen), comes
// into a V2 house the same way a V1 site does. What the house cannot ask
// a foreign machine it is told: the domain, and, when the folder does not
// say it, the command; how to stop the app for the last copy and start it
// again if the import fails are commands the operator gives, because only
// they know whether it is pm2 or a unit. Without them the copy is made
// with the app running, and a database written meanwhile may arrive torn:
// said before anything moves.

// GenericSource is an app folder on a machine that is not a V2 house.
type GenericSource struct {
	// Dir is the app's folder.
	Dir string
	// Command overrides what the folder says the app is started with.
	Command []string
	// StopCmd and StartCmd stop the app where it runs and start it
	// again (a shell line each); empty is not stopping it.
	StopCmd, StartCmd string

	manifest []byte
	docker   bool
	how      string
}

// Detect works out the V2 manifest: the folder's own (V2, or V1
// converted), or one from what is in it. It says how it decided.
func (s *GenericSource) Detect(domain string) (string, error) {
	if raw, err := os.ReadFile(filepath.Join(s.Dir, "site.json")); err == nil {
		if mf, err := manifest.Parse(raw); err == nil && mf.Validate() == nil {
			return s.set(mf, "its own site.json")
		}
		res := migrate.Convert(domain, raw)
		if res.Ready() {
			return s.set(res.Manifest, "its V1 site.json, converted")
		}
	}
	mf := &manifest.Manifest{Version: manifest.Version}
	switch {
	case len(s.Command) > 0:
		mf.Run = &manifest.Run{Command: s.Command}
		if fileIn(s.Dir, "Dockerfile") {
			mf.Run.Runtime = "docker"
			mf.Run.Docker = &manifest.Docker{Build: "Dockerfile"}
			return s.set(mf, "the command given, in a container from its Dockerfile")
		}
		return s.set(mf, "the command given")
	case fileIn(s.Dir, "Dockerfile"):
		cmd := dockerCmd(filepath.Join(s.Dir, "Dockerfile"))
		if len(cmd) == 0 {
			return "", errors.New("it has a Dockerfile whose CMD is not in exec form ([\"…\"]): say the command with --command")
		}
		mf.Run = &manifest.Run{Command: cmd, Runtime: "docker", Docker: &manifest.Docker{Build: "Dockerfile"}}
		return s.set(mf, "its Dockerfile, CMD "+strings.Join(cmd, " "))
	case npmStart(s.Dir):
		mf.Run = &manifest.Run{Command: []string{"npm", "run", "start"}}
		return s.set(mf, "package.json's start script")
	case fileIn(s.Dir, "index.html"):
		mf.Static = &manifest.Static{Root: "."}
		return s.set(mf, "an index.html: served as files")
	case fileIn(s.Dir, filepath.Join("public", "index.html")):
		mf.Static = &manifest.Static{Root: "public"}
		return s.set(mf, "public/index.html: served as files")
	}
	return "", errors.New("nothing in the folder says how it runs: say the command with --command")
}

func (s *GenericSource) set(mf *manifest.Manifest, how string) (string, error) {
	mf.Disabled = false
	raw, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return "", err
	}
	s.manifest, s.how = append(raw, '\n'), how
	s.docker = mf.Run != nil && mf.Run.Runtime == "docker"
	return how, nil
}

func fileIn(dir, name string) bool {
	info, err := os.Stat(filepath.Join(dir, name))
	return err == nil && !info.IsDir()
}

func npmStart(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	return json.Unmarshal(raw, &pkg) == nil && pkg.Scripts["start"] != ""
}

var cmdLine = regexp.MustCompile(`(?im)^\s*CMD\s+(\[.*\])\s*$`)

// dockerCmd is the Dockerfile's last CMD in exec form.
func dockerCmd(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	m := cmdLine.FindAllSubmatch(raw, -1)
	if len(m) == 0 {
		return nil
	}
	var cmd []string
	if json.Unmarshal(m[len(m)-1][1], &cmd) != nil {
		return nil
	}
	return cmd
}

// Parts implements Mover.Parts: the folder, without node_modules.
func (s *GenericSource) Parts(_, _ string) []archive.Part {
	return []archive.Part{{Name: "site", Path: s.Dir, SkipDir: func(n string) bool { return n == "node_modules" }}}
}

// Needs implements Needer.
func (s *GenericSource) Needs(string) Needs { return Needs{Docker: s.docker} }

// Stop runs the stop command, when there is one, and returns the manifest
// the V2 house commits.
func (s *GenericSource) Stop(ctx context.Context, domain string) ([]byte, error) {
	if s.manifest == nil {
		if _, err := s.Detect(domain); err != nil {
			return nil, err
		}
	}
	if s.StopCmd != "" {
		if out, err := exec.CommandContext(ctx, "sh", "-c", s.StopCmd).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("stopping it (%s): %v: %s", s.StopCmd, err, strings.TrimSpace(string(out)))
		}
	}
	return s.manifest, nil
}

// Start runs the start command, when there is one: the app back where it
// was, the import having failed.
func (s *GenericSource) Start(string, []byte) error {
	if s.StartCmd == "" {
		return nil
	}
	if out, err := exec.Command("sh", "-c", s.StartCmd).CombinedOutput(); err != nil {
		return fmt.Errorf("starting it again (%s): %v: %s", s.StartCmd, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Published implements Source.
func (s *GenericSource) Published(string) (string, bool) { return "", true }

// Names implements Source.
func (s *GenericSource) Names(string) []string { return nil }

// Unpublish implements Source.
func (s *GenericSource) Unpublish(context.Context, string) {}

// Archive implements Source: the folder is left where it was.
func (s *GenericSource) Archive(context.Context, string, []byte) (string, error) { return "", nil }
