package helper

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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/release"
)

// UpdateArgs is the input of update.install: the release's name and
// nothing else. Where it comes from is fixed (release.Repo), and it must
// be newer than what runs, so a supervisor that was broken into can
// neither install a binary of its own nor go back to an older, weaker one.
type UpdateArgs struct {
	Version string `json:"version"`
}

// UpdateResult says what was replaced.
type UpdateResult struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Binary string `json:"binary"`
}

var updateHTTP = &http.Client{Timeout: 10 * time.Minute}

// binaryPath is the executable to replace: this one, which is also the
// supervisor's (one binary, two units).
func (s *Server) binaryPath() (string, error) {
	if s.Binary != "" {
		return s.Binary, nil
	}
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

func (s *Server) updateInstall(ctx context.Context, raw json.RawMessage) (any, error) {
	var a UpdateArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if !release.Valid(a.Version) {
		return nil, fmt.Errorf("%w: %q is not a release name", errBadArgument, a.Version)
	}
	if !release.Newer(a.Version, s.Build) {
		return nil, fmt.Errorf("%s is not newer than the running %s", a.Version, s.Build)
	}
	bin, err := s.binaryPath()
	if err != nil {
		return nil, err
	}
	arch := s.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}
	asset := release.Asset(arch)
	sum, err := fetchSum(ctx, release.URL(a.Version, asset+".sha256"))
	if err != nil {
		return nil, err
	}
	// Next to the target, so the rename is atomic and a half-written
	// binary is never what the next start runs.
	tmp, err := os.CreateTemp(filepath.Dir(bin), ".middleware-update-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	got, err := download(ctx, release.URL(a.Version, asset), tmp)
	tmp.Close()
	if err != nil {
		return nil, err
	}
	if got != sum {
		return nil, fmt.Errorf("%s: sha256 %s does not match the published %s", asset, got, sum)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return nil, err
	}
	// It must run here and be the release asked for: an arm64 binary on
	// amd64, or a mislabelled asset, is found now and not at the restart.
	vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, tmp.Name(), "version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "middleware "+a.Version {
		return nil, fmt.Errorf("the downloaded binary does not say it is %s: %v %s", a.Version, err, strings.TrimSpace(string(out)))
	}
	if err := os.Rename(tmp.Name(), bin); err != nil {
		return nil, err
	}
	// The helper is running the old binary: it ends after answering and
	// systemd (Restart=always) starts the new one.
	exit := s.Exit
	if exit == nil {
		exit = func() { os.Exit(0) }
	}
	go func() {
		time.Sleep(time.Second)
		s.Log.Printf("updated to %s; restarting", a.Version)
		exit()
	}()
	return UpdateResult{From: s.Build, To: a.Version, Binary: bin}, nil
}

func fetchSum(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := updateHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(body))
	if len(f) == 0 || len(f[0]) != 64 {
		return "", errors.New("the published sha256 does not read")
	}
	return strings.ToLower(f[0]), nil
}

func download(ctx context.Context, url string, w io.Writer) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := updateHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", url, resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, 512<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// UpdateInstall asks the helper to install a release; it downloads it
// itself, so the call takes as long as the download.
func (c *Client) UpdateInstall(ctx context.Context, version string) (UpdateResult, error) {
	var r UpdateResult
	long := &Client{path: c.path, timeout: 10 * time.Minute}
	err := long.Call(ctx, OpUpdateInstall, UpdateArgs{Version: version}, &r)
	return r, err
}
