package helper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	s := &Server{Build: "v2.0.0-alpha.5", Binary: bin, Arch: "amd64", Exit: func() { exited <- struct{}{} }, Log: log.New(io.Discard, "", 0), Exec: &Recorder{}}
	return s, bin, exited
}

// restarted is the helper systemd starts after an install: the new
// binary, on the same file and record.
func restarted(old *Server, build string) (*Server, *Recorder, chan struct{}) {
	rec := &Recorder{Outputs: map[string]string{}}
	exited := make(chan struct{}, 1)
	s := &Server{Build: build, Binary: old.Binary, Arch: "amd64", Exit: func() { exited <- struct{}{} }, Log: log.New(io.Discard, "", 0), Exec: rec, WatchEvery: 5 * time.Millisecond}
	return s, rec, exited
}

func call(s *Server, version string) Response {
	args, _ := json.Marshal(UpdateArgs{Version: version})
	return s.Handle(context.Background(), Peer{}, Request{Op: OpUpdateInstall, Args: args})
}

func confirm(s *Server, version string) (ConfirmResult, Response) {
	args, _ := json.Marshal(ConfirmArgs{Version: version})
	r := s.Handle(context.Background(), Peer{}, Request{Op: OpUpdateConfirm, Args: args})
	var res ConfirmResult
	if r.OK {
		json.Unmarshal(r.Result, &res)
	}
	return res, r
}

func readState(t *testing.T, bin string) UpdateState {
	t.Helper()
	var st UpdateState
	if b, err := os.ReadFile(bin + StateSuffix); err == nil {
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// watch runs the trial as the restarted helper does and waits for it.
func watch(t *testing.T, s *Server) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); s.WatchUpdate(context.Background()) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the trial never ended")
	}
}

func contains(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func TestAnUpdateKeepsThePreviousBinaryAndPutsTheNewOneOnTrial(t *testing.T) {
	fakeReleases(t, "v2.0.0-alpha.6", false)
	s, bin, exited := updateServer(t)
	before := time.Now()
	if r := call(s, "v2.0.0-alpha.6"); !r.OK {
		t.Fatalf("refused: %s", r.Error)
	}
	<-exited
	if b, _ := os.ReadFile(bin + PrevSuffix); string(b) != "old" {
		t.Fatalf("kept %q", b)
	}
	if fi, err := os.Stat(bin + PrevSuffix); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("kept binary: %v %v", fi, err)
	}
	p := readState(t, bin).Pending
	if p == nil || p.From != "v2.0.0-alpha.5" || p.To != "v2.0.0-alpha.6" || p.Deadline.Before(before.Add(DefaultUpdateGrace)) {
		t.Fatalf("record %+v", p)
	}
	// What is on trial is not installed over: a rollback would go back to
	// the binary nobody confirmed.
	fakeReleases(t, "v2.0.0-alpha.7", false)
	s2, _, _ := restarted(s, "v2.0.0-alpha.6")
	if r := call(s2, "v2.0.0-alpha.7"); r.OK || !strings.Contains(r.Error, "not confirmed") {
		t.Fatalf("installed over the trial: %+v", r)
	}
}

// The data and the directory entries reach the disk before the update
// is answered: the downloaded file before it replaces the binary, the
// kept one before the record says it exists, the folder after the rename.
func TestAnUpdateSyncsTheFilesAndTheFolderItRenamesIn(t *testing.T) {
	fakeReleases(t, "v2.0.0-alpha.6", false)
	s, bin, exited := updateServer(t)
	type synced struct{ name, bin string }
	var got []synced
	old := syncFile
	syncFile = func(f *os.File) error {
		b, _ := os.ReadFile(bin)
		got = append(got, synced{filepath.Base(f.Name()), string(b)})
		return f.Sync()
	}
	t.Cleanup(func() { syncFile = old })
	if r := call(s, "v2.0.0-alpha.6"); !r.OK {
		t.Fatalf("refused: %s", r.Error)
	}
	<-exited
	dir := filepath.Base(filepath.Dir(bin))
	var download, prev, folderAfter bool
	for _, g := range got {
		switch {
		case strings.HasPrefix(g.name, ".middleware-update-") && g.bin == "old":
			download = true
		case strings.HasPrefix(g.name, ".middleware-prev-") && g.bin == "old":
			prev = true
		case g.name == dir && strings.Contains(g.bin, "v2.0.0-alpha.6"):
			folderAfter = true
		}
	}
	if !download || !prev || !folderAfter {
		t.Fatalf("download %v, kept %v, folder after the rename %v: %+v", download, prev, folderAfter, got)
	}
}

func TestTheNewSupervisorConfirmsAndNothingIsRolledBack(t *testing.T) {
	fakeReleases(t, "v2.0.0-alpha.6", false)
	s, bin, exited := updateServer(t)
	if r := call(s, "v2.0.0-alpha.6"); !r.OK {
		t.Fatalf("refused: %s", r.Error)
	}
	<-exited
	s2, rec, exited2 := restarted(s, "v2.0.0-alpha.6")
	// The old supervisor, still on its way out, cannot confirm the new one.
	if _, r := confirm(s2, "v2.0.0-alpha.5"); r.OK {
		t.Fatal("the old version confirmed the new one")
	}
	if _, r := confirm(s2, "dev"); r.OK {
		t.Fatal("a name that is not a release was taken")
	}
	res, r := confirm(s2, "v2.0.0-alpha.6")
	if !r.OK || !res.Confirmed || res.Skipped != "" {
		t.Fatalf("%+v %+v", res, r)
	}
	if _, err := os.Stat(bin + StateSuffix); !os.IsNotExist(err) {
		t.Fatalf("the record is still there: %v", err)
	}
	watch(t, s2)
	if b, _ := os.ReadFile(bin); !strings.Contains(string(b), "v2.0.0-alpha.6") {
		t.Fatalf("the binary is %q", b)
	}
	if contains(rec.Lines(), "systemctl restart --no-block middleware.service") {
		t.Fatal("restarted the supervisor")
	}
	select {
	case <-exited2:
		t.Fatal("the helper ended")
	default:
	}
	// Every start confirms; with nothing on trial that is a no.
	if res, r := confirm(s2, "v2.0.0-alpha.6"); !r.OK || res.Confirmed {
		t.Fatalf("%+v %+v", res, r)
	}
}

// Unconfirmed past the deadline, the helper goes back on its own: the
// kept binary returns, the supervisor is restarted on it, the helper ends
// to be started on it too. The release it came from is not installed
// again, by anyone, until a newer one is out.
func TestAnUnconfirmedUpdateRollsBackAndIsNotInstalledAgain(t *testing.T) {
	fakeReleases(t, "v2.0.0-alpha.6", false)
	s, bin, exited := updateServer(t)
	s.UpdateGrace = 30 * time.Millisecond
	if r := call(s, "v2.0.0-alpha.6"); !r.OK {
		t.Fatalf("refused: %s", r.Error)
	}
	<-exited
	s2, rec, exited2 := restarted(s, "v2.0.0-alpha.6")
	watch(t, s2)
	<-exited2
	if b, _ := os.ReadFile(bin); string(b) != "old" {
		t.Fatalf("the binary is %q", b)
	}
	lines := rec.Lines()
	if !contains(lines, "systemctl reset-failed middleware.service") || !contains(lines, "systemctl restart --no-block middleware.service") {
		t.Fatalf("the supervisor was not restarted: %v", lines)
	}
	st := readState(t, bin)
	if st.Pending != nil || st.Skipped != "v2.0.0-alpha.6" || st.Rollback == nil || st.Rollback.From != "v2.0.0-alpha.6" || st.Rollback.To != "v2.0.0-alpha.5" {
		t.Fatalf("record %+v", st)
	}

	// The old helper is back: nothing on trial, the bad release refused.
	s3, _, _ := restarted(s, "v2.0.0-alpha.5")
	watch(t, s3)
	if r := call(s3, "v2.0.0-alpha.6"); r.OK || !strings.Contains(r.Error, "rolled back") {
		t.Fatalf("the skipped release was installed again: %+v", r)
	}
	if b, _ := os.ReadFile(bin); string(b) != "old" {
		t.Fatalf("the binary is %q", b)
	}
	// The old supervisor hears of it once, and of the skip every time.
	res, r := confirm(s3, "v2.0.0-alpha.5")
	if !r.OK || res.Confirmed || res.Skipped != "v2.0.0-alpha.6" || res.Rollback == nil {
		t.Fatalf("%+v %+v", res, r)
	}
	if res, _ := confirm(s3, "v2.0.0-alpha.5"); res.Rollback != nil || res.Skipped != "v2.0.0-alpha.6" {
		t.Fatalf("second confirmation %+v", res)
	}
	// A newer release is installed, and once it serves the skip is over.
	fakeReleases(t, "v2.0.0-alpha.7", false)
	if r := call(s3, "v2.0.0-alpha.7"); !r.OK {
		t.Fatalf("the newer release was refused: %s", r.Error)
	}
	s4, _, _ := restarted(s, "v2.0.0-alpha.7")
	if res, r := confirm(s4, "v2.0.0-alpha.7"); !r.OK || !res.Confirmed || res.Skipped != "" {
		t.Fatalf("%+v %+v", res, r)
	}
}

// restartingExec is a supervisor in a crash loop: every look finds it
// restarted once more.
type restartingExec struct {
	*Recorder
	n atomic.Int32
}

func (e *restartingExec) Run(ctx context.Context, name string, args []string, stdin string) (string, error) {
	if name == "systemctl" && len(args) > 0 && args[0] == "show" {
		return fmt.Sprintf("NRestarts=%d\nActiveState=activating\n", e.n.Add(1)), nil
	}
	return e.Recorder.Run(ctx, name, args, stdin)
}

// A supervisor that keeps restarting, or that systemd gave up on, is not
// waited for until the deadline.
func TestACrashLoopingSupervisorIsRolledBackBeforeTheDeadline(t *testing.T) {
	for name, exec := range map[string]func(*Recorder) Exec{
		"restarting": func(r *Recorder) Exec { return &restartingExec{Recorder: r} },
		"failed": func(r *Recorder) Exec {
			r.Outputs["systemctl show middleware.service --property=NRestarts,ActiveState"] = "NRestarts=4\nActiveState=failed\n"
			return r
		},
	} {
		t.Run(name, func(t *testing.T) {
			fakeReleases(t, "v2.0.0-alpha.6", false)
			s, bin, exited := updateServer(t)
			s.UpdateGrace = time.Hour
			if r := call(s, "v2.0.0-alpha.6"); !r.OK {
				t.Fatalf("refused: %s", r.Error)
			}
			<-exited
			s2, rec, exited2 := restarted(s, "v2.0.0-alpha.6")
			s2.Exec = exec(rec)
			watch(t, s2)
			<-exited2
			if b, _ := os.ReadFile(bin); string(b) != "old" {
				t.Fatalf("the binary is %q", b)
			}
			if !contains(rec.Lines(), "systemctl restart --no-block middleware.service") {
				t.Fatalf("not restarted: %v", rec.Lines())
			}
		})
	}
}

// A record that names another binary than the one running is not acted
// on: the binary was put there some other way, and going back from it
// would undo the operator's own install.
func TestARecordForAnotherBinaryIsDroppedNotRolledBack(t *testing.T) {
	fakeReleases(t, "v2.0.0-alpha.6", false)
	s, bin, exited := updateServer(t)
	s.UpdateGrace = time.Millisecond
	if r := call(s, "v2.0.0-alpha.6"); !r.OK {
		t.Fatalf("refused: %s", r.Error)
	}
	<-exited
	os.WriteFile(bin, []byte("by hand"), 0o755)
	s2, rec, exited2 := restarted(s, "v2.0.0-alpha.8")
	watch(t, s2)
	if b, _ := os.ReadFile(bin); string(b) != "by hand" {
		t.Fatalf("the binary is %q", b)
	}
	if len(rec.Lines()) != 0 {
		t.Fatalf("ran %v", rec.Lines())
	}
	select {
	case <-exited2:
		t.Fatal("the helper ended")
	default:
	}
	if st := readState(t, bin); st.Pending != nil {
		t.Fatalf("record %+v", st)
	}
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
