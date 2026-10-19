// Package sites is the write side of sites/: creating, editing, stopping,
// archiving and inspecting a site folder on behalf of the API. The engine
// only reads manifests; everything the panel and the CLI change goes
// through here so that every change is validated, written atomically and
// announced on the bus.
package sites

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/engine"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/manifest"
)

// Errors callers branch on.
var (
	ErrNotFound    = errors.New("no such site")
	ErrExists      = errors.New("a folder with that name already exists")
	ErrBadDomain   = errors.New("not a valid domain name (lower-case ASCII, at least one dot)")
	ErrUnmanaged   = errors.New("the folder has no site.json")
	ErrOutside     = errors.New("path leaves the site folder")
	ErrBadTemplate = errors.New("unknown template: want static, node, go or docker")
)

// Manager operates on the sites of one installation.
type Manager struct {
	Root   string
	Engine *engine.Engine // may be nil: then nothing is alive and restarts fail
	Bus    *events.Bus
	// Ctx bounds background work (deploys); nil means Background.
	Ctx context.Context

	deploys deployState
}

// New builds a manager over root. bus may be nil for tests.
func New(root string, eng *engine.Engine, bus *events.Bus) *Manager {
	if bus == nil {
		bus = events.New("")
	}
	// Absolute, because the root reaches a site's environment as
	// MIDDLEWARE_SITE_DIR and a build step may chdir before using it.
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &Manager{Root: root, Engine: eng, Bus: bus}
}

// Site is the API's view of one folder, as docs/api.md describes it.
type Site struct {
	Domain   string                  `json:"domain"`
	Mode     manifest.Mode           `json:"mode"`
	Alive    bool                    `json:"alive"`
	Port     int                     `json:"port"`
	Revision string                  `json:"revision"`
	Handing  bool                    `json:"handing"`
	Error    string                  `json:"error"`
	Managed  bool                    `json:"managed"`
	Manifest *manifest.Manifest      `json:"manifest,omitempty"`
	Workers  map[string]WorkerStatus `json:"workers,omitempty"`
}

// WorkerStatus is a worker's state.
type WorkerStatus struct {
	Port  int    `json:"port"`
	Alive bool   `json:"alive"`
	Error string `json:"error"`
}

// SitesDir is sites/ under the root.
func (m *Manager) SitesDir() string { return filepath.Join(m.Root, "sites") }

// Dir is a site's folder.
func (m *Manager) Dir(domain string) string { return filepath.Join(m.SitesDir(), domain) }

func (m *Manager) logsDir(domain string) string {
	return filepath.Join(m.Root, "data", "logs", domain)
}

func (m *Manager) emit(typ, domain, actor, msg string, data map[string]any) {
	if actor == "" {
		actor = "operator"
	}
	m.Bus.Emit(events.Event{Type: typ, Subject: domain, Actor: actor, Message: msg, Data: data})
}

func (m *Manager) lookup(domain string) (engine.Snapshot, bool) {
	if m.Engine == nil {
		return engine.Snapshot{}, false
	}
	return m.Engine.Lookup(domain)
}

// List returns every folder under sites/ whose name is a domain: what the
// engine runs, what it has not reconciled yet, and folders without a
// manifest, which are listed as unmanaged so a folder that is ignored
// never looks the same as one that is served.
func (m *Manager) List() ([]Site, error) {
	entries, err := os.ReadDir(m.SitesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []Site{}, nil
		}
		return nil, err
	}
	out := make([]Site, 0, len(entries))
	for _, ent := range entries {
		if !ent.IsDir() && ent.Type()&os.ModeSymlink == 0 {
			continue
		}
		if !manifest.ValidDomain(ent.Name()) {
			continue
		}
		out = append(out, m.describe(ent.Name()))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out, nil
}

// Get returns one site.
func (m *Manager) Get(domain string) (Site, error) {
	if !manifest.ValidDomain(domain) {
		return Site{}, ErrNotFound
	}
	st, err := os.Stat(m.Dir(domain))
	if err != nil || !st.IsDir() {
		return Site{}, ErrNotFound
	}
	return m.describe(domain), nil
}

// describe merges the engine's snapshot with what is on disk. The disk is
// read even when the engine knows the site: a manifest written a second
// ago is what the operator expects to see back, not the one from the
// last tick.
func (m *Manager) describe(domain string) Site {
	s := Site{Domain: domain, Mode: manifest.ModeStatic}
	dir := m.Dir(domain)
	if !fsx.Exists(filepath.Join(dir, manifest.FileName)) {
		s.Managed = false
		s.Mode = ""
		return s
	}
	s.Managed = true
	mf, err := manifest.Load(dir)
	if err != nil {
		s.Error = err.Error()
	} else {
		s.Manifest = mf
		s.Mode = mf.Mode()
	}
	sn, ok := m.lookup(domain)
	if !ok {
		return s
	}
	if s.Manifest == nil && sn.Manifest != nil {
		s.Manifest = sn.Manifest // the last valid one, still in force
		s.Mode = sn.Mode
	}
	s.Alive, s.Port, s.Revision, s.Handing = sn.Alive, sn.Port, sn.Revision, sn.Handing
	if s.Error == "" {
		s.Error = sn.Error
	}
	if len(sn.Workers) > 0 {
		s.Workers = map[string]WorkerStatus{}
		for name, w := range sn.Workers {
			s.Workers[name] = WorkerStatus{Port: w.Port, Alive: w.Alive, Error: w.Error}
		}
	}
	return s
}

// Create makes sites/<domain>/ with the given manifest, or from a template
// when mf is nil. The manifest is written last: the engine ticks at any
// moment, and a manifest pointing at files that are not there yet would
// start a crash loop before the folder is complete.
func (m *Manager) Create(domain string, mf *manifest.Manifest, template string) (Site, error) {
	if !manifest.ValidDomain(domain) {
		return Site{}, ErrBadDomain
	}
	dir := m.Dir(domain)
	if fsx.Exists(dir) {
		return Site{}, ErrExists
	}
	var files map[string]string
	if mf == nil {
		if template == "" {
			template = "static"
		}
		t, ok := templates[template]
		if !ok {
			return Site{}, ErrBadTemplate
		}
		mf, files = t(domain)
	}
	if mf.Version == 0 {
		mf.Version = manifest.Version
	}
	if err := mf.Validate(); err != nil {
		return Site{}, fmt.Errorf("manifest: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Site{}, err
	}
	for name, content := range files {
		if err := fsx.WriteAtomic(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			_ = os.RemoveAll(dir)
			return Site{}, err
		}
	}
	if err := manifest.Save(dir, mf); err != nil {
		_ = os.RemoveAll(dir)
		return Site{}, err
	}
	m.emit("site.created", domain, "", "site created", map[string]any{"template": template, "mode": string(mf.Mode())})
	return m.describe(domain), nil
}

// Update replaces the manifest.
func (m *Manager) Update(domain string, mf *manifest.Manifest) (Site, error) {
	dir, err := m.managedDir(domain)
	if err != nil {
		return Site{}, err
	}
	if mf.Version == 0 {
		mf.Version = manifest.Version
	}
	if err := manifest.Save(dir, mf); err != nil {
		return Site{}, fmt.Errorf("manifest: %w", err)
	}
	m.emit("manifest.updated", domain, "", "manifest replaced", nil)
	return m.describe(domain), nil
}

// Patch merges top-level keys into the manifest; a null value removes the
// key. The result goes through the same parser as a file edit, so an
// unknown key is rejected rather than silently dropped.
func (m *Manager) Patch(domain string, partial map[string]json.RawMessage) (Site, error) {
	dir, err := m.managedDir(domain)
	if err != nil {
		return Site{}, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, manifest.FileName))
	if err != nil {
		return Site{}, err
	}
	current := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &current); err != nil {
		return Site{}, fmt.Errorf("current manifest: %w", err)
	}
	keys := make([]string, 0, len(partial))
	for k, v := range partial {
		keys = append(keys, k)
		if string(v) == "null" {
			delete(current, k)
		} else {
			current[k] = v
		}
	}
	sort.Strings(keys)
	merged, err := json.Marshal(current)
	if err != nil {
		return Site{}, err
	}
	mf, err := manifest.Parse(merged)
	if err != nil {
		return Site{}, fmt.Errorf("manifest: %w", err)
	}
	if err := manifest.Save(dir, mf); err != nil {
		return Site{}, err
	}
	m.emit("manifest.updated", domain, "", "manifest patched: "+strings.Join(keys, ", "), map[string]any{"keys": keys})
	return m.describe(domain), nil
}

// SetDisabled stops or starts a site by writing the decision to its
// manifest, where it survives a restart.
func (m *Manager) SetDisabled(domain string, disabled bool) (Site, error) {
	dir, err := m.managedDir(domain)
	if err != nil {
		return Site{}, err
	}
	mf, err := manifest.Load(dir)
	if err != nil {
		return Site{}, fmt.Errorf("manifest: %w", err)
	}
	mf.Disabled = disabled
	if err := manifest.Save(dir, mf); err != nil {
		return Site{}, err
	}
	if disabled {
		m.emit("site.stopped", domain, "", "stopped by the operator", nil)
	} else {
		m.emit("site.started", domain, "", "started by the operator", nil)
	}
	return m.describe(domain), nil
}

// SetHold puts a site behind the holding page, or takes it out.
func (m *Manager) SetHold(domain string, hold manifest.Hold) (Site, error) {
	dir, err := m.managedDir(domain)
	if err != nil {
		return Site{}, err
	}
	mf, err := manifest.Load(dir)
	if err != nil {
		return Site{}, fmt.Errorf("manifest: %w", err)
	}
	if hold.Enabled {
		mf.Hold = &hold
	} else {
		mf.Hold = nil
	}
	if err := manifest.Save(dir, mf); err != nil {
		return Site{}, err
	}
	m.emit("site.hold", domain, "", fmt.Sprintf("hold %v", hold.Enabled), map[string]any{"enabled": hold.Enabled, "title": hold.Title})
	return m.describe(domain), nil
}

// Restart forces a handover.
func (m *Manager) Restart(ctx context.Context, domain string) error {
	if _, err := m.managedDir(domain); err != nil {
		return err
	}
	if m.Engine == nil {
		return errors.New("no engine")
	}
	return m.Engine.Restart(ctx, domain)
}

// ArchiveDir is where Delete with archive=true moves folders.
func (m *Manager) ArchiveDir() string { return filepath.Join(m.Root, "data", "archive") }

// Delete removes a site folder, or moves it under data/archive/ with a
// timestamp. The instance is stopped first by writing disabled into the
// manifest and waiting for the engine: removing the folder under a running
// process would work on Linux, but a worker on a written port would keep
// that port until the engine noticed the folder was gone, and a site
// created again with the same name would then fail to bind.
func (m *Manager) Delete(ctx context.Context, domain string, archive bool) (archived string, err error) {
	if !manifest.ValidDomain(domain) {
		return "", ErrNotFound
	}
	dir := m.Dir(domain)
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
		return "", ErrNotFound
	}
	original, _ := os.ReadFile(filepath.Join(dir, manifest.FileName))
	if err := m.stopAndWait(ctx, domain, dir, original); err != nil {
		return "", err
	}
	if !archive {
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
		_ = os.RemoveAll(m.logsDir(domain))
		m.emit("site.deleted", domain, "", "folder deleted", nil)
		return "", nil
	}
	if err := os.MkdirAll(m.ArchiveDir(), 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(m.ArchiveDir(), domain+"-"+time.Now().UTC().Format("20060102T150405Z"))
	if err := os.Rename(dir, dest); err != nil {
		return "", fmt.Errorf("archive: %w (sites/ and data/ must be on the same filesystem)", err)
	}
	if original != nil {
		// Put the manifest back the way it was: the archive is a copy of
		// what the operator had, not of the stop we did to get there.
		_ = fsx.WriteAtomic(filepath.Join(dest, manifest.FileName), original, 0o644)
	}
	m.emit("site.archived", domain, "", "folder moved to "+dest, map[string]any{"path": dest})
	return dest, nil
}

// stopAndWait writes disabled into the manifest and waits for the engine
// to report nothing alive. Without a manifest or an engine there is
// nothing to stop.
func (m *Manager) stopAndWait(ctx context.Context, domain, dir string, original []byte) error {
	sn, ok := m.lookup(domain)
	if !ok || original == nil {
		return nil
	}
	running := sn.Alive
	for _, w := range sn.Workers {
		running = running || w.Alive
	}
	if !running {
		return nil
	}
	mf, err := manifest.Parse(original)
	if err != nil {
		return fmt.Errorf("cannot stop %s before removing it: %w", domain, err)
	}
	mf.Disabled = true
	if err := manifest.Save(dir, mf); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		sn, ok := m.lookup(domain)
		stopped := !ok || !sn.Alive
		for _, w := range sn.Workers {
			stopped = stopped && !w.Alive
		}
		if stopped {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s: the engine did not stop the instance in time", domain)
}

// managedDir returns the folder of a site that has a manifest.
func (m *Manager) managedDir(domain string) (string, error) {
	if !manifest.ValidDomain(domain) {
		return "", ErrNotFound
	}
	dir := m.Dir(domain)
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return "", ErrNotFound
	}
	if !fsx.Exists(filepath.Join(dir, manifest.FileName)) {
		return "", ErrUnmanaged
	}
	return dir, nil
}
