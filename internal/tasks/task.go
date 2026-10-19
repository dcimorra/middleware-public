// Package tasks runs tasks/<name>.json on schedule. A task is a command
// that happens at its hour, inside a site's folder with that site's
// environment when it names one, as a process of its own group with its
// output in a file: the same rules a site instance runs under, through
// the same runtime, so a task cannot hang the supervisor any more than a
// site can.
package tasks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/cron"
	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/manifest"
)

// Version is the only supported schema version.
const Version = 2

// Task is tasks/<name>.json, as docs/manifest.md describes it.
type Task struct {
	Version  int               `json:"version"`
	Schedule string            `json:"schedule"`
	Site     string            `json:"site,omitempty"`
	Command  []string          `json:"command"`
	Timeout  manifest.Duration `json:"timeout,omitempty"`
	Overlap  string            `json:"overlap,omitempty"` // "skip" (default) | "queue"
	Env      map[string]string `json:"env,omitempty"`
	Disabled bool              `json:"disabled,omitempty"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidName reports whether s can name a task file.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// Parse decodes and validates a task. Unknown fields are an error for the
// same reason they are in a manifest: "scheduel" silently never running is
// worse than a refusal.
func Parse(data []byte) (*Task, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var t Task
	if err := dec.Decode(&t); err != nil {
		return nil, err
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// Validate checks the task is something the scheduler could run.
func (t *Task) Validate() error {
	if t.Version != Version {
		return fmt.Errorf("version: want %d, got %d", Version, t.Version)
	}
	if _, err := cron.Parse(t.Schedule); err != nil {
		return fmt.Errorf("schedule: %w", err)
	}
	if len(t.Command) == 0 || strings.TrimSpace(t.Command[0]) == "" {
		return errors.New("command: empty")
	}
	if t.Site != "" && !manifest.ValidDomain(t.Site) {
		return fmt.Errorf("site: %q is not a domain", t.Site)
	}
	switch t.Overlap {
	case "", "skip", "queue":
	default:
		return fmt.Errorf("overlap: %q is not skip or queue", t.Overlap)
	}
	if t.Timeout < 0 {
		return errors.New("timeout: negative")
	}
	return nil
}

// Path is where a task file lives.
func Path(root, name string) string { return filepath.Join(root, "tasks", name+".json") }

// Load reads and validates one task.
func Load(root, name string) (*Task, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("%q is not a task name", name)
	}
	data, err := os.ReadFile(Path(root, name))
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Save writes a task atomically after validating it.
func Save(root, name string, t *Task) error {
	if !ValidName(name) {
		return fmt.Errorf("%q is not a task name", name)
	}
	if err := t.Validate(); err != nil {
		return err
	}
	return fsx.WriteJSON(Path(root, name), t, 0o644)
}

// Names lists the task files under root, sorted by the directory walk.
func Names(root string) []string {
	ents, err := os.ReadDir(filepath.Join(root, "tasks"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		n := strings.TrimSuffix(e.Name(), ".json")
		if e.IsDir() || n == e.Name() || !ValidName(n) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// Run statuses.
const (
	StatusOK      = "ok"
	StatusFailed  = "failed"
	StatusTimeout = "timeout"
	StatusSkipped = "skipped"
	StatusError   = "error" // could not even start
)

// Run is one execution, as recorded in data/tasks/<name>.json.
type Run struct {
	At       time.Time         `json:"at"`
	Status   string            `json:"status"`
	Duration manifest.Duration `json:"duration"`
	ExitCode int               `json:"exit_code"`
	Trigger  string            `json:"trigger"` // "schedule" | "operator"
	Error    string            `json:"error,omitempty"`
}

// Record is the run history of one task.
type Record struct {
	Name string `json:"name"`
	Runs []Run  `json:"runs"`
}

// KeepRuns is how many runs a record holds.
const KeepRuns = 50

// RecordPath is where a task's history lives.
func RecordPath(root, name string) string {
	return filepath.Join(root, "data", "tasks", name+".json")
}

// LogPath is where a task's output goes.
func LogPath(root, name string) string {
	return filepath.Join(root, "data", "logs", "tasks", name+".log")
}

// LoadRecord reads a task's history; a missing file is an empty record.
func LoadRecord(root, name string) Record {
	var r Record
	if err := fsx.ReadJSON(RecordPath(root, name), &r); err != nil {
		r = Record{}
	}
	r.Name = name
	if r.Runs == nil {
		r.Runs = []Run{}
	}
	return r
}

func appendRun(root, name string, run Run) error {
	r := LoadRecord(root, name)
	r.Runs = append(r.Runs, run)
	if len(r.Runs) > KeepRuns {
		r.Runs = r.Runs[len(r.Runs)-KeepRuns:]
	}
	return fsx.WriteJSON(RecordPath(root, name), r, 0o644)
}
