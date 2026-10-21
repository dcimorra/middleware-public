package update

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/release"
)

func fakeGitHub(t *testing.T, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	old := release.APIBase
	release.APIBase = srv.URL
	t.Cleanup(func() { release.APIBase = old })
}

const releases = `[
 {"tag_name":"v2.0.0-alpha.10","assets":[{"name":"middleware-linux-amd64"}]},
 {"tag_name":"v2.0.0-alpha.11","draft":true,"assets":[{"name":"middleware-linux-amd64"}]},
 {"tag_name":"v2.0.0-alpha.12","assets":[{"name":"middleware-linux-arm64"}]},
 {"tag_name":"v2.0.0-alpha.9","assets":[{"name":"middleware-linux-amd64"}]},
 {"tag_name":"v1.9.0","assets":[{"name":"middleware-linux-amd64"}]}
]`

func TestTheNewestPublishedReleaseForThisMachineIsOffered(t *testing.T) {
	fakeGitHub(t, releases)
	u := New(Options{Current: "v2.0.0-alpha.5", Arch: "amd64", Install: func(context.Context, string) error { return nil }})
	st := u.Check(context.Background(), true)
	// Not the draft, not the one without an amd64 binary; alpha.10 is
	// after alpha.9 although it sorts before it as text.
	if st.Error != "" || st.Latest != "v2.0.0-alpha.10" || !st.Available || !st.CanInstall {
		t.Fatalf("%+v", st)
	}
	// Who runs a stable release is offered stable ones only.
	stable := New(Options{Current: "v1.8.0", Arch: "amd64"})
	if st := stable.Check(context.Background(), true); st.Latest != "v1.9.0" {
		t.Fatalf("stable: %+v", st)
	}
}

func TestInstallAsksTheHelperForTheNewestAndRestarts(t *testing.T) {
	fakeGitHub(t, releases)
	var asked string
	restarted := make(chan struct{}, 1)
	u := New(Options{Current: "v2.0.0-alpha.5", Arch: "amd64",
		Install: func(_ context.Context, v string) error { asked = v; return nil },
		Restart: func() { restarted <- struct{}{} }})
	v, err := u.Install(context.Background(), "operator")
	if err != nil || v != "v2.0.0-alpha.10" || asked != v {
		t.Fatalf("%q %v asked %q", v, err, asked)
	}
	<-restarted
	// Nothing newer: refused, the helper is not asked.
	current := New(Options{Current: "v2.0.0-alpha.10", Arch: "amd64", Install: func(context.Context, string) error { t.Fatal("asked"); return nil }})
	if _, err := current.Install(context.Background(), "operator"); err != ErrNothingNewer {
		t.Fatalf("%v", err)
	}
}

func TestADevBuildOrNoHelperCannotInstall(t *testing.T) {
	fakeGitHub(t, releases)
	if st := New(Options{Current: "dev", Install: func(context.Context, string) error { return nil }}).Status(); st.CanInstall {
		t.Fatal("dev can install")
	}
	if st := New(Options{Current: "v2.0.0-alpha.5", Install: func(context.Context, string) error { return nil }, Helper: func() bool { return false }}).Status(); st.CanInstall || st.Why == "" {
		t.Fatalf("no helper: %+v", st)
	}
	if st := New(Options{Current: "v2.0.0-alpha.5", Auto: func() bool { return false }}).Status(); st.Auto {
		t.Fatal("auto on by default")
	}
}

// A check that finds a release installs it with auto on; with auto off it
// says so once, however many checks find the same release.
func TestACheckInstallsOnlyWithAutoOnAndOtherwiseAnnouncesOnce(t *testing.T) {
	fakeGitHub(t, releases)
	bus := events.New("")
	ch, stop := bus.Subscribe()
	defer stop()
	auto := false
	var installed string
	u := New(Options{Current: "v2.0.0-alpha.5", Arch: "amd64", Bus: bus, Auto: func() bool { return auto },
		Every:   func() time.Duration { return 5 * time.Minute },
		Install: func(_ context.Context, v string) error { installed = v; return nil }})
	if st := u.Status(); st.EveryMinutes != 5 {
		t.Fatalf("interval %+v", st)
	}
	u.tick(context.Background())
	u.tick(context.Background())
	if installed != "" {
		t.Fatal("installed with auto off")
	}
	announced := 0
	for len(ch) > 0 {
		if e := <-ch; e.Type == "update.available" {
			announced++
		}
	}
	if announced != 1 {
		t.Fatalf("announced %d times", announced)
	}
	auto = true
	u.tick(context.Background())
	if installed != "v2.0.0-alpha.10" {
		t.Fatalf("auto on did not install: %q", installed)
	}
}
