package relocate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/tasks"
)

// A V1 house keeps its tasks in tasks/<name>.json at its root, each naming
// the site it runs in. An import used to take the folder and leave them
// there: the site arrived serving and without its nightly jobs, and the V1
// copies, stopped with their site but never told, were one "disabled" away
// from running against the same database as the V2 ones.

// v1Task is a V1 task file. Unknown fields are an error: a field V2 does
// not carry, dropped in silence, is a task that does something else.
type v1Task struct {
	Title    string            `json:"title"`
	Group    string            `json:"group"`
	Note     string            `json:"note"`
	Schedule string            `json:"schedule"`
	Command  []string          `json:"command"`
	Site     string            `json:"site"`
	Env      map[string]string `json:"env"`
	Timezone string            `json:"timezone"`
	Timeout  string            `json:"timeout"`
	Overlap  string            `json:"overlap"`
	Disabled bool              `json:"disabled"`
}

// v1Timeout is what V1 gives a task that says none; V2 gives it no limit.
const v1Timeout = "30m"

// ConvertV1Task turns a V1 task file into a V2 one. dir is the site's
// folder, zone the V1 machine's timezone ("" when it cannot be told).
func ConvertV1Task(raw []byte, dir, zone string) ([]byte, []string, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var v1 v1Task
	if err := dec.Decode(&v1); err != nil {
		return nil, nil, err
	}
	var notes []string
	t := tasks.Task{
		Version: tasks.Version, Schedule: v1.Schedule, Site: v1.Site, Env: v1.Env,
		Disabled: v1.Disabled, Title: v1.Title, Note: v1.Note, Timezone: v1.Timezone,
	}
	t.Command = append([]string(nil), v1.Command...)
	if t.Timezone == "" {
		// V1 read it in its machine's clock, which the target may not
		// share: written down, so the hour stays the hour it was.
		t.Timezone = zone
	}
	timeout := v1.Timeout
	if timeout == "" {
		timeout = v1Timeout
	}
	if err := t.Timeout.UnmarshalJSON([]byte(`"` + timeout + `"`)); err != nil {
		return nil, nil, fmt.Errorf("timeout: %w", err)
	}
	switch strings.ToLower(v1.Overlap) {
	case "", "saltar", "skip":
	case "paralelo":
		// V2 never runs two of the same task at once; queued, none is lost.
		t.Overlap = "queue"
		notes = append(notes, "overlap paralelo becomes queue: V2 runs one at a time")
	default:
		return nil, nil, fmt.Errorf("overlap: %q", v1.Overlap)
	}
	if len(t.Command) > 0 && bareProgram(t.Command[0]) {
		// V1 found it in its own PATH (a global tsx of this machine's
		// nvm); the target has its own. The site's copy is installed
		// there with its dependencies.
		local := filepath.Join("node_modules", ".bin", t.Command[0])
		if fsx.Exists(filepath.Join(dir, local)) {
			notes = append(notes, fmt.Sprintf("%s is the site's own ./%s", t.Command[0], local))
			t.Command[0] = "./" + local
		} else {
			notes = append(notes, fmt.Sprintf("runs %s from the machine's PATH: it must be there", t.Command[0]))
		}
	}
	out, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	out = append(out, '\n')
	if _, err := tasks.Parse(out); err != nil {
		return nil, nil, err
	}
	return out, notes, nil
}

// bareProgram is a command V1 resolved with its PATH, other than the ones
// V2 resolves the same way everywhere (the site's Node, a shell).
func bareProgram(c string) bool {
	if strings.ContainsRune(c, '/') {
		return false
	}
	switch c {
	case "node", "npm", "npx", "sh", "bash", "env":
		return false
	}
	return true
}

// localZone is this machine's timezone name, or "" when it cannot be told.
func localZone() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		return tz
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if _, zone, ok := strings.Cut(link, "zoneinfo/"); ok {
			return zone
		}
	}
	if raw, err := os.ReadFile("/etc/timezone"); err == nil {
		return strings.TrimSpace(string(raw))
	}
	return ""
}

// v1TaskFiles are the V1 task files that run in domain, by name.
func (s *V1Source) v1TaskFiles(domain string) (map[string][]byte, error) {
	ents, err := os.ReadDir(filepath.Join(s.Root, "tasks"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if e.IsDir() || !ok {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.Root, "tasks", e.Name()))
		if err != nil {
			return nil, err
		}
		var head struct {
			Site string `json:"site"`
		}
		if err := json.Unmarshal(raw, &head); err != nil || head.Site != domain {
			continue
		}
		if !tasks.ValidName(name) {
			return nil, fmt.Errorf("task %s: V2 cannot name a task that way", name)
		}
		out[name] = raw
	}
	return out, nil
}

// Tasks implements Tasker: the site's V1 tasks, converted.
func (s *V1Source) Tasks(domain string) (map[string][]byte, error) {
	out, _, err := s.convertTasks(domain)
	return out, err
}

// TaskNotes says what the conversion of the site's tasks does, for the
// plan an import shows before it moves anything.
func (s *V1Source) TaskNotes(domain string) ([]string, error) {
	_, notes, err := s.convertTasks(domain)
	return notes, err
}

func (s *V1Source) convertTasks(domain string) (map[string][]byte, []string, error) {
	raw, err := s.v1TaskFiles(domain)
	if err != nil {
		return nil, nil, err
	}
	if len(raw) == 0 {
		return nil, nil, nil
	}
	dir := filepath.Join(s.Root, "sites", domain)
	zone := localZone()
	out := map[string][]byte{}
	said := map[string][]string{}
	on := 0
	for _, name := range SortedNames(raw) {
		v2, notes, err := ConvertV1Task(raw[name], dir, zone)
		if err != nil {
			return nil, nil, fmt.Errorf("task %s: %w", name, err)
		}
		out[name] = v2
		if t, _ := tasks.Parse(v2); t != nil && !t.Disabled {
			on++
		}
		for _, n := range notes {
			said[n] = append(said[n], name)
		}
	}
	notes := []string{fmt.Sprintf("tasks: %d go with it (%d on, %d off), and are turned off here", len(out), on, len(out)-on)}
	keys := make([]string, 0, len(said))
	for k := range said {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		notes = append(notes, fmt.Sprintf("tasks: %s (%d)", k, len(said[k])))
	}
	return out, notes, nil
}

// HoldTasks implements Tasker: disabled written into each V1 task that is
// on, which is how V1 turns one off. The file is kept as it was, to be put
// back word for word.
func (s *V1Source) HoldTasks(domain string) ([]string, error) {
	raw, err := s.v1TaskFiles(domain)
	if err != nil {
		return nil, err
	}
	if s.heldTasks == nil {
		s.heldTasks = map[string][]byte{}
	}
	var held []string
	for _, name := range SortedNames(raw) {
		var m map[string]any
		if err := json.Unmarshal(raw[name], &m); err != nil {
			return held, fmt.Errorf("task %s: %w", name, err)
		}
		if off, _ := m["disabled"].(bool); off {
			continue
		}
		m["disabled"] = true
		out, _ := json.MarshalIndent(m, "", "  ")
		if err := fsx.WriteAtomic(filepath.Join(s.Root, "tasks", name+".json"), append(out, '\n'), 0o644); err != nil {
			return held, fmt.Errorf("task %s: %w", name, err)
		}
		s.heldTasks[name] = raw[name]
		held = append(held, name)
	}
	return held, nil
}

// ReleaseTasks implements Tasker: each held task file back as it was.
func (s *V1Source) ReleaseTasks(names []string) error {
	var failed []string
	for _, name := range names {
		raw, ok := s.heldTasks[name]
		if !ok {
			continue
		}
		if err := fsx.WriteAtomic(filepath.Join(s.Root, "tasks", name+".json"), raw, 0o644); err != nil {
			failed = append(failed, name+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%s", strings.Join(failed, "; "))
	}
	return nil
}
