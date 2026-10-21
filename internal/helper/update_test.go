package helper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/release"
)

// fakeReleases serves a release whose binary is a script that says its
// version, and its sha256 (or a wrong one).
func fakeReleases(t *testing.T, says string, badSum bool) {
	t.Helper()
	bin := "#!/bin/sh\necho middleware " + says + "\n"
	sum := sha256.Sum256([]byte(bin))
	hexSum := hex.EncodeToString(sum[:])
	if badSum {
		hexSum = strings.Repeat("0", 64)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/middleware-linux-amd64"):
			io.WriteString(w, bin)
		case strings.HasSuffix(r.URL.Path, "/middleware-linux-amd64.sha256"):
			io.WriteString(w, hexSum+"  middleware-linux-amd64\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := release.DownloadBase
	release.DownloadBase = srv.URL
	t.Cleanup(func() { release.DownloadBase = old })
}

func updateServer(t *testing.T) (*Server, string, chan struct{}) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "middleware")
	os.WriteFile(bin, []byte("old"), 0o755)
	exited := make(chan struct{}, 1)
	s := &Server{Build: "v2.0.0-alpha.5", Binary: bin, Arch: "amd64", Exit: func() { exited <- struct{}{} }, Log: log.New(io.Discard, "", 0)}
	return s, bin, exited
}

func call(s *Server, version string) Response {
	args, _ := json.Marshal(UpdateArgs{Version: version})
	return s.Handle(context.Background(), Peer{}, Request{Op: OpUpdateInstall, Args: args})
}

func TestAnUpdateInstallsTheVerifiedReleaseAndRestartsTheHelper(t *testing.T) {
	fakeReleases(t, "v2.0.0-alpha.6", false)
	s, bin, exited := updateServer(t)
	if r := call(s, "v2.0.0-alpha.6"); !r.OK {
		t.Fatalf("refused: %s", r.Error)
	}
	if b, _ := os.ReadFile(bin); !strings.Contains(string(b), "v2.0.0-alpha.6") {
		t.Fatalf("the binary is %q", b)
	}
	<-exited
}

func TestAnUpdateRefusesWhatIsNotANewerVerifiedRelease(t *testing.T) {
	for name, c := range map[string]struct {
		says, ask string
		badSum    bool
	}{
		"older":          {"v2.0.0-alpha.4", "v2.0.0-alpha.4", false},
		"the same":       {"v2.0.0-alpha.5", "v2.0.0-alpha.5", false},
		"not a release":  {"x", "v2.0.0/../../evil", false},
		"wrong sha256":   {"v2.0.0-alpha.6", "v2.0.0-alpha.6", true},
		"another inside": {"v2.0.0-alpha.7", "v2.0.0-alpha.6", false},
	} {
		t.Run(name, func(t *testing.T) {
			fakeReleases(t, c.says, c.badSum)
			s, bin, _ := updateServer(t)
			if r := call(s, c.ask); r.OK {
				t.Fatal("installed")
			}
			if b, _ := os.ReadFile(bin); string(b) != "old" {
				t.Fatalf("the binary changed to %q", b)
			}
			if m, _ := filepath.Glob(filepath.Join(filepath.Dir(bin), ".middleware-update-*")); len(m) > 0 {
				t.Fatalf("left %v", m)
			}
		})
	}
}
