package installer

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/shots"
)

func fakeChromeForTesting(t *testing.T) *httptest.Server {
	t.Helper()
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	add := func(name string, mode os.FileMode, body string) {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(mode)
		w, _ := zw.CreateHeader(h)
		w.Write([]byte(body))
	}
	add("chrome-headless-shell-linux64/chrome-headless-shell", 0o755, "#!/bin/sh\necho Chrome 1.0\n")
	add("chrome-headless-shell-linux64/deb.deps", 0o644, "libasound2 (>= 1.0.17)\nlibgtk-3-0 (>= 3.9.10) | libgtk-4-1\nlibnss3 (>= 2:3.35)\nwget\n")
	zw.Close()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"channels":{"Stable":{"version":"1.0","downloads":{"chrome-headless-shell":[{"platform":"linux64","url":"%s/shell.zip"}]}}}}`, srv.URL)
	})
	mux.HandleFunc("/shell.zip", func(w http.ResponseWriter, r *http.Request) { w.Write(zbuf.Bytes()) })
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	old, oldHTTP := BrowserIndex, browserHTTP
	BrowserIndex, browserHTTP = srv.URL+"/index.json", srv.Client()
	t.Cleanup(func() { BrowserIndex, browserHTTP = old, oldHTTP })
	return srv
}

func TestABrowserAlreadyThereIsNotInstalledAgain(t *testing.T) {
	in, rec, _ := testInstaller(t)
	in.Params.Browser = "yes"
	in.FindBrowser = func(string) string { return "/usr/bin/chromium" }
	changed, detail, err := in.stepBrowser()
	if err != nil || changed || !strings.Contains(detail, "/usr/bin/chromium") {
		t.Fatalf("%v %q %v", changed, detail, err)
	}
	if len(rec.Calls) != 0 {
		t.Fatalf("it ran %v", rec.Calls)
	}
}

func TestNoBrowserIsInstalledUnlessTheOperatorSaidYes(t *testing.T) {
	fakeChromeForTesting(t)
	in, rec, _ := testInstaller(t)
	changed, detail, err := in.stepBrowser()
	if err != nil || changed || !strings.Contains(detail, "--browser yes") {
		t.Fatalf("%v %q %v", changed, detail, err)
	}
	if len(rec.Calls) != 0 {
		t.Fatalf("it ran %v", rec.Calls)
	}
}

func TestSayingYesInstallsTheHeadlessShellAndTheLibrariesItLoads(t *testing.T) {
	fakeChromeForTesting(t)
	in, rec, _ := testInstaller(t)
	in.Params.Browser = "yes"
	in.BrowserPlatform = "linux64"
	// Only what is in the root: this machine's own browsers do not count.
	in.FindBrowser = func(root string) string {
		m, _ := filepath.Glob(filepath.Join(shots.BrowserDir(root), "*", "chrome-headless-shell"))
		if len(m) == 0 {
			return ""
		}
		return m[0]
	}
	os.MkdirAll(in.Params.Root, 0o755)
	// Ubuntu 24.04: the old name is only virtual now.
	rec.Set("apt-cache show --no-all-versions libasound2", "", errors.New("purely virtual"))
	changed, detail, err := in.stepBrowser()
	if err != nil || !changed {
		t.Fatalf("%v %q %v", changed, detail, err)
	}
	bin := filepath.Join(shots.BrowserDir(in.Params.Root), "chrome-headless-shell-linux64", "chrome-headless-shell")
	if st, err := os.Stat(bin); err != nil || st.Mode()&0o100 == 0 {
		t.Fatalf("the browser is not there, executable: %v", err)
	}
	if got := shots.FindIn(in.Params.Root); got != bin && os.Getenv("MIDDLEWARE_CHROME") == "" {
		t.Fatalf("the house finds %q", got)
	}
	want := "sudo apt-get install -y -q libasound2t64 libnss3"
	found := false
	for _, c := range rec.Calls {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %q in %v", want, rec.Calls)
	}
	// A second run finds it and does nothing.
	rec.Calls = nil
	if changed, _, _ := in.stepBrowser(); changed || len(rec.Calls) != 0 {
		t.Fatalf("installed twice: %v", rec.Calls)
	}
}

// BROWSER=1 downloads the real build from Google into a temporary root
// and starts it. The libraries are not installed (that needs root): the
// machine running it must have them.
func TestTheRealHeadlessShellDownloadsAndStarts(t *testing.T) {
	if os.Getenv("BROWSER") != "1" {
		t.Skip("BROWSER=1 downloads over 100 MB from Google")
	}
	root := t.TempDir()
	in := New(Params{Root: root, Browser: "yes"})
	in.Exec = &noSudo{System{}}
	in.Out = os.Stderr
	in.FindBrowser = func(root string) string {
		m, _ := filepath.Glob(filepath.Join(shots.BrowserDir(root), "*", "chrome-headless-shell"))
		if len(m) == 0 {
			return ""
		}
		return m[0]
	}
	changed, detail, err := in.stepBrowser()
	if err != nil || !changed {
		t.Fatalf("%v %q %v", changed, detail, err)
	}
	t.Log(detail)
}

// noSudo runs everything but what needs root.
type noSudo struct{ System }

func (noSudo) Sudo(name string, args ...string) (string, error) { return "", nil }

func TestADownloadSaysHowItGoes(t *testing.T) {
	var log bytes.Buffer
	p := newProgress(&log, false)
	p.total = 100
	for i := 0; i < 10; i++ {
		p.Write(make([]byte, 10))
	}
	if n := strings.Count(log.String(), "\n"); n != 10 || !strings.Contains(log.String(), "100%") {
		t.Fatalf("in a log:\n%s", log.String())
	}
	var term bytes.Buffer
	p = newProgress(&term, true)
	p.total = 100
	p.Write(make([]byte, 50))
	p.done()
	if !strings.Contains(term.String(), "\r  [###############...............]  50%") {
		t.Fatalf("on a terminal: %q", term.String())
	}
}
