package installer

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/shots"
)

// BrowserWhy is what the operator is told before being asked: what the
// browser is for and what installing it costs, so the answer is informed.
const BrowserWhy = "The panel shows a picture of each site, and taking it needs a headless Chromium. This machine has none."

// BrowserIndex is Chrome for Testing's list of current builds, by channel
// and platform. Google publishes the headless shell there for linux64 and
// linux-arm64; the distributions do not agree on a package (Ubuntu's
// "chromium" is a snap that cannot write outside a home folder).
var BrowserIndex = "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json"

// browserHTTP fetches the index and the build; a build is over 100 MB, so
// only the headers have a deadline.
var browserHTTP = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Second, Proxy: http.ProxyFromEnvironment}}

// Packages from the build's own dependency list that a headless shell
// never loads: a desktop toolkit and helpers for a browser with a window.
var browserDepsSkipped = []string{"libgtk", "xdg-utils", "wget", "libcurl", "libc6", "glibc"}

// stepBrowser installs a headless Chromium when the operator said yes and
// there is none. One already there (the system's, Playwright's, an
// earlier install) is used as it is: nothing is installed twice.
func (in *Installer) stepBrowser() (bool, string, error) {
	find := in.FindBrowser
	if find == nil {
		find = shots.FindIn
	}
	if bin := find(in.Params.Root); bin != "" {
		return false, "present: " + bin, nil
	}
	if in.Params.Browser != "yes" {
		return false, "none: the panel shows no site pictures; `middleware install --browser yes` installs one", nil
	}
	cmd, pm := in.packageManager()
	switch pm {
	case "apk", "pacman":
		// musl, or a rolling distribution whose chromium is a plain
		// package: Google's glibc build is the wrong thing on the first,
		// and needless on the second.
		args := append(append([]string{}, cmd[1:]...), "chromium")
		if _, err := in.Exec.Sudo(cmd[0], args...); err != nil {
			return false, "", err
		}
		if bin := find(in.Params.Root); bin != "" {
			return true, "installed chromium with " + pm + ": " + bin, nil
		}
		return false, "", errors.New("chromium was installed but no headless Chromium is found")
	}
	platform := map[string]string{"amd64": "linux64", "arm64": "linux-arm64"}[runtime.GOARCH]
	if in.BrowserPlatform != "" {
		platform = in.BrowserPlatform
	}
	if platform == "" {
		return false, "", fmt.Errorf("no headless Chromium is published for %s", runtime.GOARCH)
	}
	version, url, err := browserBuild(platform)
	if err != nil {
		return false, "", err
	}
	fmt.Fprintf(in.Out, "  downloading Chrome for Testing %s (%s, over 100 MB)…\n", version, platform)
	bin, err := installBrowser(url, shots.BrowserDir(in.Params.Root), newProgress(in.Out, in.Live))
	if err != nil {
		return false, "", err
	}
	// The libraries it loads, from the list the build itself carries.
	if deps := browserDeps(filepath.Dir(bin), pm, in.Exec); len(deps) > 0 && cmd != nil {
		args := append(append([]string{}, cmd[1:]...), deps...)
		fmt.Fprintf(in.Out, "  installing the %d system libraries it loads with %s…\n", len(deps), pm)
		if _, err := in.Exec.Sudo(cmd[0], args...); err != nil {
			return false, "", fmt.Errorf("the libraries it needs: %w", err)
		}
	}
	if out, err := in.Exec.Run(bin, "--version"); err != nil {
		return false, "", fmt.Errorf("installed, but it does not start: %v %s", err, strings.TrimSpace(out))
	}
	return true, fmt.Sprintf("Chrome for Testing %s: %s", version, bin), nil
}

func browserBuild(platform string) (version, url string, err error) {
	resp, err := browserHTTP.Get(BrowserIndex)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("%s answered %s", BrowserIndex, resp.Status)
	}
	var idx struct {
		Channels map[string]struct {
			Version   string `json:"version"`
			Downloads map[string][]struct {
				Platform string `json:"platform"`
				URL      string `json:"url"`
			} `json:"downloads"`
		} `json:"channels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&idx); err != nil {
		return "", "", err
	}
	st := idx.Channels["Stable"]
	for _, d := range st.Downloads["chrome-headless-shell"] {
		if d.Platform == platform && strings.HasPrefix(d.URL, "https://") {
			return st.Version, d.URL, nil
		}
	}
	return "", "", fmt.Errorf("Chrome for Testing has no headless shell for %s", platform)
}

// installBrowser downloads the zip and unpacks it into dir, through a
// temporary folder renamed into place: a half-unpacked browser is never
// what the supervisor finds.
func installBrowser(url, dir string, prog *progress) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	zf, err := os.CreateTemp(dir, ".download-*.zip")
	if err != nil {
		return "", err
	}
	defer os.Remove(zf.Name())
	defer zf.Close()
	resp, err := browserHTTP.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", url, resp.Status)
	}
	prog.total = resp.ContentLength
	size, err := io.Copy(io.MultiWriter(zf, prog), resp.Body)
	prog.done()
	if err != nil {
		return "", err
	}
	fmt.Fprint(prog.out, "  unpacking…\n")
	zr, err := zip.NewReader(zf, size)
	if err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(dir, ".unpack-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	top := ""
	for _, f := range zr.File {
		name := filepath.Clean(f.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("the archive names %q, outside its folder", f.Name)
		}
		first, _, _ := strings.Cut(filepath.ToSlash(name), "/")
		if top == "" {
			top = first
		} else if first != top {
			return "", errors.New("the archive is not one folder")
		}
		target := filepath.Join(tmp, name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
			continue
		}
		if f.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		if err := unzipOne(f, target); err != nil {
			return "", err
		}
	}
	if !strings.HasPrefix(top, "chrome-headless-shell-") {
		return "", fmt.Errorf("the archive holds %q, not a headless shell", top)
	}
	final := filepath.Join(dir, top)
	if err := os.RemoveAll(final); err != nil {
		return "", err
	}
	if err := os.Rename(filepath.Join(tmp, top), final); err != nil {
		return "", err
	}
	bin := filepath.Join(final, "chrome-headless-shell")
	if _, err := os.Stat(bin); err != nil {
		return "", errors.New("the archive has no chrome-headless-shell")
	}
	return bin, nil
}

func unzipOne(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	mode := f.Mode().Perm() | 0o600
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// browserDeps reads the dependency list the build ships (deb.deps or
// rpm.deps) and returns the packages to ask the package manager for.
// On apt, a library renamed in the 64-bit time transition (libasound2 is
// libasound2t64 on Ubuntu 24.04) is asked for by the name that exists.
func browserDeps(dir, pm string, ex Exec) []string {
	file := map[string]string{"apt-get": "deb.deps", "dnf": "rpm.deps", "zypper": "rpm.deps"}[pm]
	if file == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		name, _, _ := strings.Cut(line, "|")
		name, _, _ = strings.Cut(strings.TrimSpace(name), " (")
		name = strings.TrimSpace(name)
		if name == "" || skippedDep(name) {
			continue
		}
		if pm == "apt-get" {
			name = aptName(name, ex)
			if name == "" {
				continue
			}
		}
		out = append(out, name)
	}
	return out
}

func skippedDep(name string) bool {
	for _, s := range browserDepsSkipped {
		if strings.HasPrefix(name, s) {
			return true
		}
	}
	return false
}

// aptName is the package apt can install for name: itself, or its t64
// successor when name is only virtual now; "" when neither exists.
//
// The exit status does not say it: for a purely virtual package
// (libasound2 on Ubuntu 24.04) apt-cache show prints a notice and exits 0,
// and the install that followed failed with "has no installation
// candidate" (a fresh VPS, 21 October 2026). A real one prints its record.
func aptName(name string, ex Exec) string {
	for _, n := range []string{name, name + "t64"} {
		out, err := ex.Run("apt-cache", "show", "--no-all-versions", n)
		if err == nil && hasRecord(out, n) {
			return n
		}
	}
	return ""
}

func hasRecord(out, name string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "Package: "+name {
			return true
		}
	}
	return false
}

// progress says how a download goes. On a terminal it is one line
// rewritten in place; in a log, a line every 10%, so whoever reads it
// later sees it moved and where it stopped. A download of a minute with
// nothing on screen looked like a hung installer.
type progress struct {
	out   io.Writer
	live  bool
	total int64
	got   int64
	shown int64 // last percentage (or MB, without a total) printed
	at    time.Time
}

func newProgress(out io.Writer, live bool) *progress {
	if out == nil {
		out = io.Discard
	}
	return &progress{out: out, live: live, shown: -1}
}

func (p *progress) Write(b []byte) (int, error) {
	p.got += int64(len(b))
	mb := float64(p.got) / (1 << 20)
	if p.total > 0 {
		pct := p.got * 100 / p.total
		switch {
		case p.live && (pct != p.shown || time.Since(p.at) > time.Second):
			const width = 30
			fill := int(pct) * width / 100
			fmt.Fprintf(p.out, "\r  [%s%s] %3d%%  %.0f / %.0f MB", strings.Repeat("#", fill), strings.Repeat(".", width-fill), pct, mb, float64(p.total)/(1<<20))
			p.shown, p.at = pct, time.Now()
		case !p.live && pct/10 != p.shown/10:
			fmt.Fprintf(p.out, "  %d%%  %.0f MB\n", pct, mb)
			p.shown = pct
		}
		return len(b), nil
	}
	// No length announced: megabytes so far.
	if step := p.got >> 23; step != p.shown { // every 8 MB
		if p.live {
			fmt.Fprintf(p.out, "\r  %.0f MB", mb)
		} else {
			fmt.Fprintf(p.out, "  %.0f MB\n", mb)
		}
		p.shown = step
	}
	return len(b), nil
}

// done ends the line a terminal was rewriting.
func (p *progress) done() {
	if p.live && p.shown >= 0 {
		fmt.Fprint(p.out, "\n")
	}
}
