package migrate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/manifest"
)

// BackupName is where the V1 manifest is kept once a site is converted:
// next to the new one, so going back is renaming a file.
const BackupName = "site.v1.json"

// Plan converts every site folder under sitesDir without writing anything.
// Folders already on V2 are left out; a folder whose name cannot be a site
// or project is reported as blocked rather than skipped, because a site
// the new engine would silently ignore is a site that vanished.
func Plan(sitesDir string) ([]Result, error) {
	entries, err := os.ReadDir(sitesDir)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, e := range entries {
		if !e.IsDir() && e.Type()&os.ModeSymlink == 0 {
			continue
		}
		name := e.Name()
		data, err := os.ReadFile(filepath.Join(sitesDir, name, manifest.FileName))
		if err != nil {
			continue // not a site: no manifest
		}
		if isV2(data) {
			continue
		}
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			// V1 never loaded these (retired sites are kept as
			// _retirado-<domain>-<date>), and V2 does not either.
			out = append(out, Result{Name: name, Skipped: "V1 ignored this folder; V2 archives under data/archive instead"})
			continue
		}
		if !manifest.ValidName(name) {
			out = append(out, Result{Name: name, Blockers: []string{"the folder name is not a domain nor a project name"}})
			continue
		}
		r := Convert(name, data)
		if r.Ready() {
			if err := r.Manifest.CheckInside(filepath.Join(sitesDir, name)); err != nil {
				r.Blockers = append(r.Blockers, err.Error())
				r.Manifest = nil
			}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func isV2(data []byte) bool {
	var probe struct {
		Version int `json:"version"`
	}
	return json.Unmarshal(data, &probe) == nil && probe.Version == manifest.Version
}

// Apply writes the converted manifest of every ready result, keeping the
// V1 one as site.v1.json. A site with blockers is not touched. The backup
// is written first and never overwritten, so running Apply twice cannot
// lose the original.
func Apply(sitesDir string, results []Result) (written []string, err error) {
	for _, r := range results {
		if !r.Ready() {
			continue
		}
		dir := filepath.Join(sitesDir, r.Name)
		current := filepath.Join(dir, manifest.FileName)
		backup := filepath.Join(dir, BackupName)
		data, err := os.ReadFile(current)
		if err != nil {
			return written, err
		}
		if !fsx.Exists(backup) {
			if err := fsx.WriteAtomic(backup, data, 0o644); err != nil {
				return written, err
			}
		} else if isV2(data) {
			continue
		}
		if err := manifest.Save(dir, r.Manifest); err != nil {
			return written, errors.Join(errors.New(r.Name), err)
		}
		written = append(written, r.Name)
	}
	return written, nil
}
