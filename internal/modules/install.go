package modules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var binNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Download is the HTTP client binaries are fetched with. Tests point it at
// an httptest server; a release can take minutes on a slow link, so no
// overall timeout, only one for the headers.
var Download = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Second, Proxy: http.ProxyFromEnvironment}}

// InstallBinary fetches url into binDir/name, mode 0755, through a temp
// file renamed over the target, so a half-downloaded binary is never what
// the next start executes. sha256Hex, when given, must match the whole
// file or nothing is installed: a release page can be swapped under a
// stable URL and a module that downloads executables must not trust the
// transport alone. progress may be nil.
func InstallBinary(ctx context.Context, binDir, name, url, sha256Hex string, progress func(string)) (string, error) {
	if progress == nil {
		progress = func(string) {}
	}
	if !binNameRe.MatchString(name) {
		return "", fmt.Errorf("install: %q is not a binary name", name)
	}
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return "", fmt.Errorf("install: %q is not an http(s) url", url)
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	target := filepath.Join(binDir, name)
	progress("downloading " + url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := Download.Do(req)
	if err != nil {
		return "", fmt.Errorf("install %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("install %s: %s answered %s", name, url, resp.Status)
	}
	tmp, err := os.CreateTemp(binDir, "."+name+".*.part")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), resp.Body)
	if err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("install %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", err
	}
	if n == 0 {
		cleanup()
		return "", errors.New("install " + name + ": empty download")
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if sha256Hex != "" {
		if !strings.EqualFold(strings.TrimSpace(sha256Hex), sum) {
			cleanup()
			return "", fmt.Errorf("install %s: sha256 %s does not match the expected %s", name, sum, sha256Hex)
		}
		progress("sha256 verified")
	} else {
		progress("sha256 " + sum + " (no checksum to compare with)")
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		cleanup()
		return "", err
	}
	if err := os.Rename(tmpName, target); err != nil {
		cleanup()
		return "", err
	}
	progress(fmt.Sprintf("installed %s (%d bytes)", target, n))
	return target, nil
}

// InstallBinary is the manager's shortcut into its own bin directory.
func (m *Manager) InstallBinary(ctx context.Context, name, url, sha256Hex string, progress func(string)) (string, error) {
	return InstallBinary(ctx, m.BinDir(), name, url, sha256Hex, progress)
}
