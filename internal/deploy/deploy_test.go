package deploy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/manifest"
)

func TestMatchComparesRepositoryWithoutCaseAndBranchExactly(t *testing.T) {
	m := &manifest.Manifest{Version: 2, Deploy: &manifest.Deploy{GitHub: &manifest.GitHubDeploy{Repository: "Acme/App"}}}
	if !Match(m, "acme/app", "refs/heads/main") {
		t.Fatal("default branch is main and case does not matter")
	}
	if Match(m, "acme/app", "refs/heads/dev") || Match(m, "acme/other", "refs/heads/main") || Match(m, "acme/app", "refs/tags/v1") {
		t.Fatal("another branch, another repository or a tag must not match")
	}
	m.Deploy.GitHub.Branch = "dev"
	if !Match(m, "acme/app", "refs/heads/dev") || Match(m, "acme/app", "refs/heads/main") {
		t.Fatal("a configured branch replaces main")
	}
	if Match(&manifest.Manifest{Version: 2}, "acme/app", "refs/heads/main") {
		t.Fatal("a manifest without deploy.github never matches")
	}
}

// git runs git in dir with a fixed identity so commits work on a bare
// machine.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@test", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "HOME="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// makeRepo builds a bare repository whose main branch holds build.sh,
// a script that writes a file named after the build id.
func makeRepo(t *testing.T, script string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	base := t.TempDir()
	work := filepath.Join(base, "work")
	bare := filepath.Join(base, "origin.git")
	os.MkdirAll(work, 0o755)
	git(t, base, "init", "-q", "--bare", bare)
	git(t, work, "init", "-q")
	os.WriteFile(filepath.Join(work, "build.sh"), []byte(script), 0o755)
	os.WriteFile(filepath.Join(work, "README"), []byte("v1\n"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "first")
	git(t, work, "remote", "add", "origin", bare)
	git(t, work, "push", "-q", "origin", "main")
	return bare
}

func makeSite(t *testing.T, m *manifest.Manifest) (root, dir string) {
	t.Helper()
	root = t.TempDir()
	dir = filepath.Join(root, "sites", "app.test")
	os.MkdirAll(dir, 0o755)
	if err := manifest.Save(dir, m); err != nil {
		t.Fatal(err)
	}
	return root, dir
}

func newRunner(root, bare string, restart func(context.Context, string) error) (*Runner, *events.Bus) {
	bus := events.New("")
	r := New(Options{
		DataDir: filepath.Join(root, "data"),
		Bus:     bus,
		Restart: restart,
		GitURL:  func(string) string { return bare },
		Token:   func(string) string { return "tok-should-not-hit-disk" },
	})
	return r, bus
}

func eventTypes(bus *events.Bus) string {
	var out []string
	for _, e := range bus.Recent() {
		out = append(out, e.Type)
	}
	return strings.Join(out, ",")
}

func TestADeployClonesBuildsAndWritesThePointerOnlyAfterTheBuild(t *testing.T) {
	bare := makeRepo(t, "#!/bin/sh\necho building into $BUILD_ID\nmkdir -p \"$NEXT_DIST_DIR\" && echo built > \"$NEXT_DIST_DIR/out\"\n")
	m := &manifest.Manifest{Version: 2,
		Run:    &manifest.Run{Command: []string{"./server"}, EnvFromFile: map[string]string{"NEXT_DIST_DIR": ".current"}},
		Deploy: &manifest.Deploy{GitHub: &manifest.GitHubDeploy{Repository: "acme/app", Build: [][]string{{"./build.sh"}}, Pointer: ".current"}}}
	root, dir := makeSite(t, m)
	r, bus := newRunner(root, bare, nil)
	st, err := r.Run(context.Background(), "app.test", dir, m, "test")
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(root, "data", "logs", "app.test", "deploy.log"))
		t.Fatalf("%v\n%s", err, log)
	}
	if st.Status != "ok" || st.Commit == "" {
		t.Fatalf("status: %+v", st)
	}
	pointer, err := os.ReadFile(filepath.Join(dir, ".current"))
	if err != nil || strings.TrimSpace(string(pointer)) != st.BuildID {
		t.Fatalf("pointer must hold the build id: %q %v", pointer, err)
	}
	if _, err := os.Stat(filepath.Join(dir, st.BuildID, "out")); err != nil {
		t.Fatal("the build saw the pointer's variable set to the new build id:", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "site.json")); err != nil {
		t.Fatal("the checkout must keep the manifest")
	}
	last, err := r.Last("app.test")
	if err != nil || last.Status != "ok" || last.BuildID != st.BuildID {
		t.Fatalf("last: %+v %v", last, err)
	}
	if got := eventTypes(bus); got != "deploy.started,deploy.done" {
		t.Fatalf("events: %s", got)
	}
	// The token must not have been written anywhere in the site folder.
	config, _ := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if strings.Contains(string(config), "tok-should-not-hit-disk") {
		t.Fatal("the token reached .git/config")
	}
	log, _ := os.ReadFile(filepath.Join(root, "data", "logs", "app.test", "deploy.log"))
	if !strings.Contains(string(log), "building into "+st.BuildID) {
		t.Fatalf("the build output must be in the log:\n%s", log)
	}
	// A second deploy on the same folder fetches instead of cloning.
	st2, err := r.Run(context.Background(), "app.test", dir, m, "test")
	if err != nil || st2.Commit != st.Commit {
		t.Fatalf("second deploy: %+v %v", st2, err)
	}
}

func TestAFailedBuildLeavesThePointerAloneAndRecordsTheFailure(t *testing.T) {
	bare := makeRepo(t, "#!/bin/sh\necho boom >&2\nexit 3\n")
	m := &manifest.Manifest{Version: 2, Deploy: &manifest.Deploy{GitHub: &manifest.GitHubDeploy{Repository: "acme/app", Build: [][]string{{"./build.sh"}}, Pointer: ".current"}}}
	root, dir := makeSite(t, m)
	os.WriteFile(filepath.Join(dir, ".current"), []byte("previous\n"), 0o644)
	r, bus := newRunner(root, bare, nil)
	st, err := r.Run(context.Background(), "app.test", dir, m, "test")
	if err == nil {
		t.Fatal("a failing build must fail the deploy")
	}
	if st.Status != "failed" || !strings.Contains(st.Error, "build[0]") {
		t.Fatalf("status: %+v", st)
	}
	pointer, _ := os.ReadFile(filepath.Join(dir, ".current"))
	if strings.TrimSpace(string(pointer)) != "previous" {
		t.Fatalf("the pointer must still say previous, got %q", pointer)
	}
	if got := eventTypes(bus); got != "deploy.started,deploy.failed" {
		t.Fatalf("events: %s", got)
	}
	log, _ := os.ReadFile(st.Log)
	if !strings.Contains(string(log), "boom") {
		t.Fatalf("stderr of the build must be in the log:\n%s", log)
	}
}

func TestWithoutAPointerTheRestartHookIsCalledAfterABuild(t *testing.T) {
	bare := makeRepo(t, "#!/bin/sh\ntrue\n")
	m := &manifest.Manifest{Version: 2, Deploy: &manifest.Deploy{GitHub: &manifest.GitHubDeploy{Repository: "acme/app", Build: [][]string{{"./build.sh"}}}}}
	root, dir := makeSite(t, m)
	var restarted string
	r, _ := newRunner(root, bare, func(ctx context.Context, domain string) error { restarted = domain; return nil })
	if _, err := r.Run(context.Background(), "app.test", dir, m, "test"); err != nil {
		t.Fatal(err)
	}
	if restarted != "app.test" {
		t.Fatal("the restart hook must be called for the site")
	}
}

func TestAPointerOutsideTheSiteFolderIsRefused(t *testing.T) {
	bare := makeRepo(t, "#!/bin/sh\ntrue\n")
	m := &manifest.Manifest{Version: 2, Deploy: &manifest.Deploy{GitHub: &manifest.GitHubDeploy{Repository: "acme/app", Pointer: "../escape"}}}
	root, dir := makeSite(t, m)
	r, _ := newRunner(root, bare, nil)
	if _, err := r.Run(context.Background(), "app.test", dir, m, "test"); err == nil || !strings.Contains(err.Error(), "leaves the site folder") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sites", "escape")); err == nil {
		t.Fatal("nothing may be written above the site")
	}
}

func TestDeploysOfOneSiteRunOneAtATime(t *testing.T) {
	// The build appends to a file and sleeps; overlapping runs would
	// interleave the markers.
	bare := makeRepo(t, "#!/bin/sh\necho start >> \"$MIDDLEWARE_SITE_DIR/marks\"\nsleep 0.3\necho end >> \"$MIDDLEWARE_SITE_DIR/marks\"\n")
	m := &manifest.Manifest{Version: 2, Deploy: &manifest.Deploy{GitHub: &manifest.GitHubDeploy{Repository: "acme/app", Build: [][]string{{"./build.sh"}}}}}
	root, dir := makeSite(t, m)
	r, _ := newRunner(root, bare, nil)
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := r.Run(context.Background(), "app.test", dir, m, "test")
			done <- err
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	marks, _ := os.ReadFile(filepath.Join(dir, "marks"))
	if string(marks) != "start\nend\nstart\nend\n" {
		t.Fatalf("builds overlapped: %q", marks)
	}
}
