// Package specs is the house's side of OpenSpec (spec-driven development,
// github.com/Fission-AI/OpenSpec) in a site's project: reading what its
// openspec/ folder holds, for the panel, and setting the folder up.
//
// The CLI and the workflow are OpenSpec's own: `openspec init` writes its
// skills into the project (.claude/skills, .agents/skills…) and the
// site's agent follows them, proposing a change, waiting for a yes, then
// implementing and archiving it. What the house adds is the reading: the
// folder is plain markdown, so it is parsed here with the standard
// library rather than by running the CLI for every screen, and a site
// keeps showing its specs with the module off or the CLI gone.
package specs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Dir is the folder OpenSpec keeps everything in, at the project's root.
const Dir = "openspec"

// Overview is what a site's openspec/ folder says.
type Overview struct {
	// Initialized: the folder exists.
	Initialized bool       `json:"initialized"`
	Changes     []Change   `json:"changes"`
	Archived    []Archived `json:"archived"`
	Specs       []Spec     `json:"specs"`
}

// Change is a proposal under openspec/changes/<id>/.
type Change struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Why is the first paragraph of the proposal's "Why" section.
	Why   string `json:"why,omitempty"`
	Done  int    `json:"done"`
	Total int    `json:"total"`
	// Files are its markdown files, relative to the project's root, in
	// reading order: proposal, design, tasks, then the spec deltas.
	Files   []string  `json:"files"`
	Updated time.Time `json:"updated"`
}

// Archived is a change already applied and folded into the specs.
type Archived struct {
	ID   string `json:"id"`
	Date string `json:"date,omitempty"`
	Path string `json:"path"`
}

// Spec is one capability under openspec/specs/: how the system behaves
// today.
type Spec struct {
	Path         string `json:"path"`
	Capability   string `json:"capability"`
	Title        string `json:"title"`
	Requirements int    `json:"requirements"`
}

// A box holding only an x is done, whatever the case and the spacing
// (OpenSpec's own rule); any other box is a task not done; a line with no
// box is not a task.
var (
	taskRe = regexp.MustCompile(`^\s*[-*]\s+\[([^\]]*)\]`)
	reqRe  = regexp.MustCompile(`(?m)^###\s+Requirement:`)
	dated  = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})-(.+)$`)
)

// Read reads the project at dir. A project without openspec/ is not an
// error: Initialized says so.
func Read(dir string) (*Overview, error) {
	root := filepath.Join(dir, Dir)
	o := &Overview{Changes: []Change{}, Archived: []Archived{}, Specs: []Spec{}}
	st, err := os.Stat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return o, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a folder", Dir)
	}
	o.Initialized = true
	changes := filepath.Join(root, "changes")
	ents, _ := os.ReadDir(changes)
	for _, e := range ents {
		if !e.IsDir() || e.Name() == "archive" || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		o.Changes = append(o.Changes, readChange(dir, filepath.Join(changes, e.Name())))
	}
	sort.Slice(o.Changes, func(i, j int) bool { return o.Changes[i].Updated.After(o.Changes[j].Updated) })
	arch, _ := os.ReadDir(filepath.Join(changes, "archive"))
	for _, e := range arch {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		a := Archived{ID: e.Name(), Path: filepath.ToSlash(filepath.Join(Dir, "changes", "archive", e.Name()))}
		if m := dated.FindStringSubmatch(e.Name()); m != nil {
			a.Date, a.ID = m[1], m[2]
		}
		o.Archived = append(o.Archived, a)
	}
	sort.Slice(o.Archived, func(i, j int) bool { return o.Archived[i].Path > o.Archived[j].Path })
	specsDir := filepath.Join(root, "specs")
	_ = filepath.WalkDir(specsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "spec.md" {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		capPath, _ := filepath.Rel(specsDir, filepath.Dir(p))
		rel, _ := filepath.Rel(dir, p)
		o.Specs = append(o.Specs, Spec{
			Path: filepath.ToSlash(rel), Capability: filepath.ToSlash(capPath),
			Title: heading(raw, filepath.ToSlash(capPath)), Requirements: len(reqRe.FindAll(raw, -1)),
		})
		return nil
	})
	sort.Slice(o.Specs, func(i, j int) bool { return o.Specs[i].Capability < o.Specs[j].Capability })
	return o, nil
}

func readChange(project, dir string) Change {
	c := Change{ID: filepath.Base(dir), Title: filepath.Base(dir), Files: []string{}}
	rel := func(p string) string { r, _ := filepath.Rel(project, p); return filepath.ToSlash(r) }
	touch := func(p string) {
		if st, err := os.Stat(p); err == nil && st.ModTime().After(c.Updated) {
			c.Updated = st.ModTime()
		}
	}
	touch(dir)
	for _, name := range []string{"proposal.md", "design.md", "tasks.md"} {
		p := filepath.Join(dir, name)
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		c.Files = append(c.Files, rel(p))
		touch(p)
		switch name {
		case "proposal.md":
			c.Title = heading(raw, c.Title)
			c.Why = section(raw, "why")
		case "tasks.md":
			c.Done, c.Total = tasks(raw)
		}
	}
	var deltas []string
	_ = filepath.WalkDir(filepath.Join(dir, "specs"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".md") {
			deltas = append(deltas, rel(p))
			touch(p)
		}
		return nil
	})
	sort.Strings(deltas)
	c.Files = append(c.Files, deltas...)
	return c
}

// heading is the first "# " line, without the marks OpenSpec's templates
// put before the name ("Proposal: …", "Change: …").
func heading(raw []byte, fallback string) string {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if t, ok := strings.CutPrefix(l, "# "); ok {
			t = strings.TrimSpace(t)
			for _, p := range []string{"Proposal:", "Change:", "Spec:"} {
				t = strings.TrimSpace(strings.TrimPrefix(t, p))
			}
			if t != "" {
				return t
			}
		}
	}
	return fallback
}

// section is the first paragraph under the "## <name>" heading.
func section(raw []byte, name string) string {
	var out []string
	in := false
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(l, "#") {
			if in {
				break
			}
			in = strings.EqualFold(strings.TrimSpace(strings.TrimLeft(l, "#")), name)
			continue
		}
		if !in {
			continue
		}
		if l == "" {
			if len(out) > 0 {
				break
			}
			continue
		}
		out = append(out, l)
	}
	s := strings.Join(out, " ")
	if r := []rune(s); len(r) > 400 {
		s = string(r[:400]) + "…"
	}
	return s
}

func tasks(raw []byte) (done, total int) {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		m := taskRe.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		total++
		if strings.EqualFold(strings.TrimSpace(m[1]), "x") {
			done++
		}
	}
	return done, total
}

// Tools maps the house's agents to the names `openspec init --tools`
// knows, so each agent finds OpenSpec's skills where it looks for them.
var Tools = map[string]string{
	"claude": "claude", "openai": "codex", "agy": "antigravity", "opencode": "opencode",
	"qwen": "qwen", "kimi": "kimi", "mistral": "vibe", "xai": "grok",
}

// InitOptions say how to run `openspec init`.
type InitOptions struct {
	// Bin is the openspec executable.
	Bin string
	// Home is the HOME it runs with: the site's own, so whatever global
	// config it writes stays with the site.
	Home string
	// Path is the PATH, with the Node the CLI needs on it.
	Path string
	// Agents are the house's agents (claude, openai…); claude is always
	// included, as the panel's conversation runs it by default.
	Agents []string
}

// Init sets the project at dir up for OpenSpec. A project that already
// has the folder is left as it is: init on an existing root rewrites
// OpenSpec's skills, which `openspec update` is for.
func Init(ctx context.Context, dir string, o InitOptions) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, Dir)); err == nil {
		return "", nil
	}
	seen := map[string]bool{"claude": true}
	tools := []string{"claude"}
	for _, a := range o.Agents {
		if t := Tools[a]; t != "" && !seen[t] {
			seen[t] = true
			tools = append(tools, t)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, o.Bin, "init", "--tools", strings.Join(tools, ","), "--no-animation", ".")
	cmd.Dir = dir
	// No telemetry from a server: OpenSpec reports usage unless told not
	// to, and a site's project is nobody else's business.
	cmd.Env = []string{"HOME=" + o.Home, "PATH=" + o.Path, "OPENSPEC_TELEMETRY=0", "DO_NOT_TRACK=1", "NO_COLOR=1", "CI=1"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("openspec init: %w", err)
	}
	return string(out), nil
}
