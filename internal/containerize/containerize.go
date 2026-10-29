// Package containerize works out the container a site runs in when its
// manifest does not say one yet.
//
// A container here is not a second way of writing the site: the runtime
// mounts the folder at the same absolute path and runs the manifest's own
// command (runtime.Docker.Start). So what has to be found is only an image
// that has what the command needs, the folder brings the rest. A Dockerfile
// the site already has is that image; otherwise it follows from the
// command: Node's for node, npm and the programs in node_modules/.bin, at
// the major the site runs on today; Python's for python, at the machine's
// version; and the machine's own distribution for a compiled binary, so
// the C library it was linked against is there. Anything else it does not
// guess: it says why, and the site's agent is asked instead.
//
// What is decided here is never written straight into the manifest: the
// house starts it on a spare port first (engine.Trial), because a
// container that does not come up would leave the engine retrying it for
// ever while the old process serves.
package containerize

import (
	"bufio"
	"bytes"
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// Plan is the container decided for a site, and why.
type Plan struct {
	Docker *manifest.Docker `json:"docker"`
	How    string           `json:"how"`
}

// ErrCannot is the reason no plan could be made; what follows it is said
// to the operator and to the agent asked instead.
var ErrCannot = errors.New("cannot be worked out")

// Probe answers what the machine has; replaced by tests.
type Probe struct {
	// NodeVersion is the Node the site runs on today ("v22.11.0").
	NodeVersion func(ctx context.Context, siteDir string, argv []string) string
	// PythonVersion is the machine's python3 ("3.12.3").
	PythonVersion func(ctx context.Context) string
	// OSRelease is /etc/os-release.
	OSRelease func() []byte
}

// Machine is the Probe of this machine.
func Machine() Probe {
	return Probe{NodeVersion: nodeVersion, PythonVersion: pythonVersion, OSRelease: func() []byte {
		raw, _ := os.ReadFile("/etc/os-release")
		return raw
	}}
}

// For is the plan for a site's manifest.
func For(ctx context.Context, siteDir string, m *manifest.Manifest, p Probe) (Plan, error) {
	if m == nil || m.Run == nil || len(m.Run.Command) == 0 {
		return Plan{}, fmt.Errorf("%w: the site runs no process (files or a forward are served by the house itself)", ErrCannot)
	}
	if d := m.Run.Docker; d != nil && (d.Image != "" || d.Build != "") {
		return Plan{Docker: d, How: "the container its manifest already describes"}, nil
	}
	cwd := siteDir
	if m.Run.Cwd != "" {
		cwd = filepath.Join(siteDir, m.Run.Cwd)
	}
	for _, name := range []string{"Dockerfile", "dockerfile"} {
		rel := name
		if m.Run.Cwd != "" {
			rel = filepath.Join(m.Run.Cwd, name)
		}
		if fileThere(filepath.Join(siteDir, rel)) {
			return Plan{Docker: &manifest.Docker{Build: rel}, How: "its " + rel}, nil
		}
	}
	argv := m.Run.Command
	prog := argv[0]
	base := filepath.Base(prog)
	if strings.Contains(prog, "/") {
		path := prog
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		if strings.Contains(path, string(filepath.Separator)+"node_modules"+string(filepath.Separator)+".bin"+string(filepath.Separator)) {
			return nodePlan(ctx, siteDir, argv, p, base+" from node_modules")
		}
		if interp := shebang(path); interp != "" {
			base = interp
		} else if static, ok := elfLinking(path); ok {
			if static {
				return Plan{Docker: &manifest.Docker{Image: "gcr.io/distroless/static-debian12"}, How: prog + " is a static binary: a minimal image with certificates"}, nil
			}
			img, err := distroImage(p)
			if err != nil {
				return Plan{}, err
			}
			return Plan{Docker: &manifest.Docker{Image: img}, How: prog + " is a binary linked against the C library: " + img + ", the machine's own distribution"}, nil
		} else if !fileThere(path) {
			return Plan{}, fmt.Errorf("%w: %s is not in the folder (built at start?)", ErrCannot, prog)
		}
	}
	switch base {
	case "node", "npm", "npx", "yarn", "pnpm", "corepack":
		return nodePlan(ctx, siteDir, argv, p, base)
	case "python", "python3":
		v := ""
		if p.PythonVersion != nil {
			v = p.PythonVersion(ctx)
		}
		mm := regexp.MustCompile(`^(\d+\.\d+)`).FindString(v)
		if mm == "" {
			return Plan{}, fmt.Errorf("%w: it runs on python and this machine has no python3 to take the version from", ErrCannot)
		}
		return Plan{Docker: &manifest.Docker{Image: "python:" + mm + "-slim"}, How: "python, at " + mm + " as on this machine"}, nil
	}
	return Plan{}, fmt.Errorf("%w: nothing says which image has %q", ErrCannot, prog)
}

func nodePlan(ctx context.Context, siteDir string, argv []string, p Probe, what string) (Plan, error) {
	v := ""
	if p.NodeVersion != nil {
		v = p.NodeVersion(ctx, siteDir, argv)
	}
	major := regexp.MustCompile(`^v?(\d+)\.`).FindStringSubmatch(v)
	if major == nil {
		return Plan{}, fmt.Errorf("%w: it runs on Node and no Node was found to take the version from", ErrCannot)
	}
	return Plan{Docker: &manifest.Docker{Image: "node:" + major[1] + "-slim"}, How: what + ", on Node " + major[1] + " as it runs now"}, nil
}

// distroImage is the image of this machine's distribution, for a binary
// linked here: Debian and Ubuntu publish one per release.
func distroImage(p Probe) (string, error) {
	var raw []byte
	if p.OSRelease != nil {
		raw = p.OSRelease()
	}
	kv := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok {
			kv[k] = strings.Trim(v, `"`)
		}
	}
	switch kv["ID"] {
	case "ubuntu":
		if kv["VERSION_ID"] != "" {
			return "ubuntu:" + kv["VERSION_ID"], nil
		}
	case "debian":
		if kv["VERSION_CODENAME"] != "" {
			return "debian:" + kv["VERSION_CODENAME"] + "-slim", nil
		}
	}
	return "", fmt.Errorf("%w: a binary linked against this machine's C library, and %q publishes no image the house knows", ErrCannot, kv["ID"])
}

// shebang is the interpreter a script names (#!/usr/bin/env node is node).
func shebang(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	line, _ := bufio.NewReader(f).ReadString('\n')
	if !strings.HasPrefix(line, "#!") {
		return ""
	}
	fields := strings.Fields(strings.TrimPrefix(line, "#!"))
	if len(fields) == 0 {
		return ""
	}
	if filepath.Base(fields[0]) == "env" {
		for _, f := range fields[1:] {
			if !strings.HasPrefix(f, "-") {
				return filepath.Base(f)
			}
		}
		return ""
	}
	return filepath.Base(fields[0])
}

// elfLinking says whether path is an ELF binary, and whether it is static.
func elfLinking(path string) (static, ok bool) {
	f, err := elf.Open(path)
	if err != nil {
		return false, false
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return false, true
		}
	}
	return true, true
}

func fileThere(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func nodeVersion(ctx context.Context, siteDir string, argv []string) string {
	node := "node"
	if runtime.NodeDir != nil {
		if dir := runtime.NodeDir(ctx, siteDir, []string{"node"}); dir != "" {
			node = filepath.Join(dir, "node")
		}
	}
	return versionOf(ctx, node, "--version")
}

func pythonVersion(ctx context.Context) string {
	return strings.TrimPrefix(versionOf(ctx, "python3", "--version"), "Python ")
}

func versionOf(ctx context.Context, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
