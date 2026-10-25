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
	// A timezone must resolve on a machine without /usr/share/zoneinfo
	// too (a minimal image): a task that refuses to load there is one
	// that never runs.
	_ "time/tzdata"

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
	// Title and Note are what a person reads about the task: what it is,
	// and why it runs when and how it does. V1 tasks carry both, and a
	// note is often the only record of why the hour is that hour.
	Title string `json:"title,omitempty"`
	Note  string `json:"note,omitempty"`
	// Timezone reads the schedule in that zone ("Europe/Madrid"); empty is
	// the machine's. Without it a chain of nightly tasks written in local
	// hours moves one hour at every daylight-saving change, and two when
	// it lands on a machine set to UTC.
	Timezone string `json:"timezone,omitempty"`
}

// Location is the zone the schedule is read in.
func (t *Task) Location() *time.Location {
	if t.Timezone == "" {
		return time.Local
	}
	if loc, err := time.LoadLocation(t.Timezone); err == nil {
		return loc
	}
	return time.Local
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
	if t.Site != "" && !manifest.ValidName(t.Site) {
		return fmt.Errorf("site: %q is not a site or project", t.Site)
	}
	switch t.Overlap {
	case "", "skip", "queue":
	default:
		return fmt.Errorf("overlap: %q is not skip or queue", t.Overlap)
	}
	if t.Timeout < 0 {
		return errors.New("timeout: negative")
	}
	if t.Timezone != "" {
		if _, err := time.LoadLocation(t.Timezone); err != nil {
			return fmt.Errorf("timezone: %q is not a zone (Europe/Madrid, UTC…)", t.Timezone)
		}
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

// OfSite is every task that runs in site's folder, as its file holds it.
func OfSite(root, site string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, name := range Names(root) {
		data, err := os.ReadFile(Path(root, name))
		if err != nil {
			return nil, err
		}
		t, err := Parse(data)
		if err != nil {
			// One that does not parse is not left behind in silence: the
			// site would arrive without it and nobody would know.
			var loose struct {
				Site string `json:"site"`
			}
			if json.Unmarshal(data, &loose) == nil && loose.Site == site {
				return nil, fmt.Errorf("task %s does not read (%v): fix it or remove it before moving %s", name, err, site)
			}
			continue
		}
		if t.Site == site {
			out[name] = data
		}
	}
	return out, nil
}

// SetDisabled turns a task off or on again, keeping everything else.
func SetDisabled(root, name string, disabled bool) error {
	t, err := Load(root, name)
	if err != nil {
		return err
	}
	t.Disabled = disabled
	return Save(root, name, t)
}

// Run statuses.
const (
	StatusOK      = "ok"
	StatusFailed  = "failed"
	StatusTimeout = "timeout"
	StatusSkipped = "skipped"
	StatusError   = "error" // could not even start
	// StatusEnded is a run adopted after a restart: it ended, and its
	// exit code is not known because it was not our child.
	StatusEnded = "ended"
	// StatusStopped is a run the operator stopped.
	StatusStopped = "stopped"
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
