package deps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/fsx"
)

// Not every agent comes from npm. Two more kinds, each installed under the
// same prefix and kept current by the same daily check:
//
//   - Binary: a single executable a vendor publishes by version at a fixed
//     address (xAI's Grok Build). No checksums are published beside it, so
//     the trust is the vendor's HTTPS, as with its own installer; the
//     download's sha256 is recorded in the event so a later look can tell
//     what ran.
//   - PyPI: a Python tool installed with uv (Mistral's Vibe). uv is one
//     static binary fetched by pinned version and checked against the
//     sha256 this file carries, as Node is; it brings its own Python when
//     the machine has none that will do, which a stock Debian does not.

// Binary is a vendor's executable at a fixed address.
type Binary struct {
	// Latest answers the newest version as plain text.
	Latest string
	// URL is the download, with {version} and {arch} in it.
	URL string
	// Arch maps GOARCH to the vendor's name for it.
	Arch map[string]string
}

// uv's release, pinned, with the sha256 its release publishes.
const uvVersion = "0.13.5"

var uvSHA = map[string]string{
	"x86_64":  "f68d291c75783122dad58ab77def0e13f3233f358731db5d6d7e8715603b928f",
	"aarch64": "4bb69cf95ed13f23713bb7bc09f241896ea0c75b7c557e615a20604b82f9c403",
}

// UVDir is where uv, its tools and its Pythons live.
func (m *Manager) UVDir() string { return filepath.Join(m.Root, "modules", "uv") }

func (m *Manager) binaryVersionPath(p Package) string {
	return filepath.Join(m.BinDir(), "."+p.Name+".version")
}

var versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.]+)?$`)

func (m *Manager) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := Download.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func (m *Manager) binaryLatest(ctx context.Context, b *Binary) (string, error) {
	raw, err := m.get(ctx, b.Latest, 256)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
	if !versionRe.MatchString(v) {
		return "", fmt.Errorf("%s answered %q, not a version", b.Latest, v)
	}
	return v, nil
}

// download writes url to dst atomically and returns its sha256.
func (m *Manager) download(ctx context.Context, url, dst string, mode os.FileMode) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := Download.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", url, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (m *Manager) installBinary(ctx context.Context, p Package, want string, progress func(string)) (string, error) {
	b := p.Binary
	arch := b.Arch[runtime.GOARCH]
	if arch == "" || runtime.GOOS != "linux" {
		return "", fmt.Errorf("deps: %s publishes no build for %s/%s", p.Name, runtime.GOOS, runtime.GOARCH)
	}
	version := want
	if version == "latest" || version == "" {
		v, err := m.binaryLatest(ctx, b)
		if err != nil {
			return "", fmt.Errorf("deps: %s: %w", p.Name, err)
		}
		version = v
	}
	url := strings.NewReplacer("{version}", version, "{arch}", arch).Replace(b.URL)
	progress("downloading " + url)
	sum, err := m.download(ctx, url, filepath.Join(m.BinDir(), p.Name), 0o755)
	if err != nil {
		return "", fmt.Errorf("deps: %s: %w", p.Name, err)
	}
	progress("sha256 " + sum)
	if err := fsx.WriteAtomic(m.binaryVersionPath(p), []byte(version+"\n"), 0o644); err != nil {
		return "", err
	}
	m.emit("deps.downloaded", p.Name, "", fmt.Sprintf("%s %s downloaded from %s", p.Name, version, url), map[string]any{"version": version, "sha256": sum, "url": url})
	return version, nil
}

// uv returns the managed uv, fetching it the first time.
func (m *Manager) uv(ctx context.Context, progress func(string)) (string, error) {
	bin := filepath.Join(m.UVDir(), "bin", "uv")
	if raw, err := os.ReadFile(filepath.Join(m.UVDir(), "bin", "VERSION")); err == nil && strings.TrimSpace(string(raw)) == uvVersion {
		if _, err := os.Stat(bin); err == nil {
			return bin, nil
		}
	}
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	if arch == "" || runtime.GOOS != "linux" {
		return "", fmt.Errorf("deps: no uv download for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	name := "uv-" + arch + "-unknown-linux-gnu"
	url := "https://github.com/astral-sh/uv/releases/download/" + uvVersion + "/" + name + ".tar.gz"
	progress("downloading " + url)
	archive := filepath.Join(m.UVDir(), ".uv.tar.gz")
	sum, err := m.download(ctx, url, archive, 0o644)
	if err != nil {
		return "", fmt.Errorf("deps: uv: %w", err)
	}
	defer os.Remove(archive)
	if sum != uvSHA[arch] {
		return "", fmt.Errorf("deps: uv: sha256 %s, want %s (the download is not what astral-sh published)", sum, uvSHA[arch])
	}
	if err := untarGz(archive, filepath.Join(m.UVDir(), "bin"), name+"/"); err != nil {
		return "", err
	}
	_ = fsx.WriteAtomic(filepath.Join(m.UVDir(), "bin", "VERSION"), []byte(uvVersion+"\n"), 0o644)
	progress("uv " + uvVersion + " ready")
	return bin, nil
}

func (m *Manager) uvEnv() []string {
	env := []string{
		"HOME=" + m.UVDir(),
		"PATH=" + filepath.Join(m.UVDir(), "bin") + string(os.PathListSeparator) + "/usr/local/bin:/usr/bin:/bin",
		"UV_TOOL_DIR=" + filepath.Join(m.UVDir(), "tools"),
		"UV_TOOL_BIN_DIR=" + m.BinDir(),
		"UV_PYTHON_INSTALL_DIR=" + filepath.Join(m.UVDir(), "python"),
		"UV_CACHE_DIR=" + filepath.Join(m.UVDir(), "cache"),
		"UV_NO_PROGRESS=1",
	}
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy", "SSL_CERT_FILE"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func (m *Manager) installUV(ctx context.Context, p Package, want string, progress func(string)) (string, error) {
	uv, err := m.uv(ctx, progress)
	if err != nil {
		return "", err
	}
	spec := p.PyPI
	if want != "" && want != "latest" {
		spec += "==" + want
	}
	if err := os.MkdirAll(m.BinDir(), 0o755); err != nil {
		return "", err
	}
	progress("uv tool install " + spec)
	ictx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	// --python 3.12: the tool's minimum; uv uses the machine's when it is
	// there and fetches one when it is not.
	out, err := m.Exec(ictx, m.UVDir(), m.uvEnv(), uv, "tool", "install", "--force", "--python", "3.12", spec)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			progress(line)
		}
	}
	if err != nil {
		return "", fmt.Errorf("deps: uv tool install %s: %w", spec, err)
	}
	return m.uvVersion(p), nil
}

// uvVersion reads the installed tool's version from its dist-info.
func (m *Manager) uvVersion(p Package) string {
	norm := strings.ReplaceAll(p.PyPI, "-", "_")
	matches, _ := filepath.Glob(filepath.Join(m.UVDir(), "tools", p.PyPI, "lib", "python*", "site-packages", norm+"-*.dist-info"))
	for _, d := range matches {
		v := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(d), norm+"-"), ".dist-info")
		if versionRe.MatchString(v) {
			return v
		}
	}
	return ""
}

func (m *Manager) pypiLatest(ctx context.Context, name string) (string, error) {
	raw, err := m.get(ctx, "https://pypi.org/pypi/"+name+"/json", 8<<20)
	if err != nil {
		return "", err
	}
	var doc struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	if doc.Info.Version == "" {
		return "", errors.New("pypi has no version for " + name)
	}
	return doc.Info.Version, nil
}
