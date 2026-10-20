package engine

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/manifest"
)

// The manifest's history is kept by the engine, not by whoever writes:
// the panel, the API, a conversation's agent, a deploy and an editor over
// SSH all write site.json, and only the engine sees every one of them,
// because it reads the file on every tick. Each valid version it has not
// seen is copied to data/history/<domain>/<unix nanoseconds>.json. Only
// valid ones: an invalid manifest is never in force, so there is nothing
// to go back to in it.

// historyKeep is how many versions are kept per site.
const historyKeep = 50

// Version is one kept manifest.
type Version struct {
	ID    string    `json:"id"`
	At    time.Time `json:"at"`
	Bytes int       `json:"bytes"`
}

func (e *Engine) historyDir(domain string) string {
	return filepath.Join(e.opts.DataDir, "history", domain)
}

// ErrNoVersion is a version id that is not kept.
var ErrNoVersion = errors.New("no such manifest version")

// recordManifest keeps data if it differs from the newest kept version.
// Called with the bytes the tick just validated.
func (e *Engine) recordManifest(s *Site, data []byte) {
	sum := sha256.Sum256(data)
	if sum == s.manifestSum {
		return
	}
	s.manifestSum = sum
	dir := e.historyDir(s.Domain)
	versions := e.versions(s.Domain)
	if len(versions) > 0 {
		if last, err := os.ReadFile(filepath.Join(dir, versions[0].ID+".json")); err == nil && bytes.Equal(last, data) {
			return
		}
	}
	id := strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := fsx.WriteAtomic(filepath.Join(dir, id+".json"), data, 0o600); err != nil {
		e.emit("manifest.history", s.Domain, "warn", "could not keep this version: "+err.Error(), nil)
		return
	}
	if len(versions) >= historyKeep {
		for _, v := range versions[historyKeep-1:] {
			_ = os.Remove(filepath.Join(dir, v.ID+".json"))
		}
	}
}

// versions lists a site's kept manifests, newest first.
func (e *Engine) versions(domain string) []Version {
	entries, _ := os.ReadDir(e.historyDir(domain))
	var out []Version
	for _, en := range entries {
		id, ok := strings.CutSuffix(en.Name(), ".json")
		n, err := strconv.ParseInt(id, 10, 64)
		if !ok || err != nil {
			continue
		}
		v := Version{ID: id, At: time.Unix(0, n).UTC()}
		if info, err := en.Info(); err == nil {
			v.Bytes = int(info.Size())
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// History is a site's kept manifests, newest first; the first is the one
// in force.
func (e *Engine) History(domain string) []Version {
	if !manifest.ValidName(domain) {
		return nil
	}
	return e.versions(domain)
}

// ManifestVersion is the content of one kept version.
func (e *Engine) ManifestVersion(domain, id string) ([]byte, error) {
	if !manifest.ValidName(domain) {
		return nil, ErrNoVersion
	}
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return nil, ErrNoVersion
	}
	data, err := os.ReadFile(filepath.Join(e.historyDir(domain), id+".json"))
	if err != nil {
		return nil, ErrNoVersion
	}
	return data, nil
}
