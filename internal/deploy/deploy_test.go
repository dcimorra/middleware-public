package deploy

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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
	bare := makeRepo(t, "#!/bin/sh\necho building into $MIDDLEWARE_BUILD\nmkdir -p \"$NEXT_DIST_DIR\" && echo built > \"$NEXT_DIST_DIR/out\"\n")
	m := &manifest.Manifest{Version: 2,
		Run:    &manifest.Run{Command: []string{"./server"}, EnvFromFile: map[string]string{"NEXT_DIST_DIR": ".current"}},
		Deploy: &manifest.Deploy{GitHub: &manifest.GitHubDeploy{Repository: "acme/app", Build: [][]string{{"./build.sh"}}, Pointer: ".current"}}}
	root, dir := makeSite(t, m)
	r, bus := newRunner(root, bare, nil)
	st, err := r.Run(context.Background(), "app.test", dir, m, "test", FromGitHub)
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
	st2, err := r.Run(context.Background(), "app.test", dir, m, "test", FromGitHub)
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
	st, err := r.Run(context.Background(), "app.test", dir, m, "test", FromGitHub)
	if err == nil {
		t.Fatal("a failing build must fail the deploy")
	}
	if st.Status != "failed" || !strings.Contains(st.Error, "step 1") {
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
	m := &manifest.Manifest{Version: 2, Run: &manifest.Run{Command: []string{"./server"}}, Deploy: &manifest.Deploy{GitHub: &manifest.GitHubDeploy{Repository: "acme/app", Build: [][]string{{"./build.sh"}}}}}
	root, dir := makeSite(t, m)
	var restarted string
	r, _ := newRunner(root, bare, func(ctx context.Context, domain string) error { restarted = domain; return nil })
	if _, err := r.Run(context.Background(), "app.test", dir, m, "test", FromGitHub); err != nil {
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
	if _, err := r.Run(context.Background(), "app.test", dir, m, "test", FromGitHub); err == nil || !strings.Contains(err.Error(), "leaves the site folder") {
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
			_, err := r.Run(context.Background(), "app.test", dir, m, "test", FromGitHub)
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

// makeRepoWithManifest builds a bare repo whose main branch ships its own
// site.json (and a trivial build.sh), standing for a repository that would
// replace the local manifest on checkout.
func makeRepoWithManifest(t *testing.T, shipped *manifest.Manifest) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	base := t.TempDir()
	// A domain-named folder so manifest.Save's CheckInside is happy with a
	// run/workers manifest.
	work := filepath.Join(base, "app.test")
	bare := filepath.Join(base, "origin.git")
	os.MkdirAll(work, 0o755)
	git(t, base, "init", "-q", "--bare", bare)
	git(t, work, "init", "-q")
	if err := manifest.Save(work, shipped); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(work, "build.sh"), []byte("#!/bin/sh\ntrue\n"), 0o755)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "first")
	git(t, work, "remote", "add", "origin", bare)
	git(t, work, "push", "-q", "origin", "main")
	return bare
}

// A disabled site is a decision; a push must not revive it. The deploy is
// skipped before anything is fetched or built.
func TestADeployOfADisabledSiteIsSkipped(t *testing.T) {
	bare := makeRepo(t, "#!/bin/sh\ntrue\n")
	m := &manifest.Manifest{Version: 2, Disabled: true,
		Run:    &manifest.Run{Command: []string{"./server"}},
		Deploy: &manifest.Deploy{GitHub: &manifest.GitHubDeploy{Repository: "acme/app", Build: [][]string{{"./build.sh"}}}}}
	root, dir := makeSite(t, m)
	r, bus := newRunner(root, bare, nil)
	st, err := r.Run(context.Background(), "app.test", dir, m, "github:someone", FromGitHub)
	if err != nil {
		t.Fatalf("a skip is not an error: %v", err)
	}
	if st.Status != "skipped" {
		t.Fatalf("status: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		t.Fatal("a disabled site was fetched")
	}
	if got := eventTypes(bus); got != "deploy.skipped" {
		t.Fatalf("events: %s", got)
	}
}

// A checkout that ships the repository's own site.json must not drop the
// operator's and the guard's decisions: a hold, a stopped worker and
// under-attack mode survive the deploy even though the repository's
// manifest has none of them.
func TestACheckoutKeepsTheSiteDecisions(t *testing.T) {
	shipped := &manifest.Manifest{Version: 2,
		Run:     &manifest.Run{Command: []string{"./server"}},
		Workers: map[string]*manifest.Worker{"db": {Command: []string{"./db"}}},
		Deploy:  &manifest.Deploy{GitHub: &manifest.GitHubDeploy{Repository: "acme/app", Build: [][]string{{"./build.sh"}}}}}
	bare := makeRepoWithManifest(t, shipped)

	local := &manifest.Manifest{Version: 2,
		Run:     &manifest.Run{Command: []string{"./server"}},
		Workers: map[string]*manifest.Worker{"db": {Command: []string{"./db"}, Disabled: true}},
		Hold:    &manifest.Hold{Enabled: true, Mode: "maintenance"},
		Deploy:  &manifest.Deploy{GitHub: &manifest.GitHubDeploy{Repository: "acme/app", Build: [][]string{{"./build.sh"}}}}}
	root, dir := makeSite(t, local)
	r, _ := newRunner(root, bare, nil)
	if _, err := r.Run(context.Background(), "app.test", dir, local, "github:someone", FromGitHub); err != nil {
		log, _ := os.ReadFile(filepath.Join(root, "data", "logs", "app.test", "deploy.log"))
		t.Fatalf("%v\n%s", err, log)
	}
	after, err := manifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.Hold == nil || !after.Hold.Enabled {
		t.Fatalf("the hold was dropped: %+v", after.Hold)
	}
	if w := after.Workers["db"]; w == nil || !w.Disabled {
		t.Fatalf("the stopped worker was started: %+v", w)
	}
	// The repository's own content still arrived.
	if _, err := os.Stat(filepath.Join(dir, "build.sh")); err != nil {
		t.Fatal("the checkout did not bring the repository's files")
	}
}

// A push that lands while the operator's Deploy is building must still get
// built: Start and Trigger share one "run again" loop. Without it the
// trigger set a flag only Trigger watched, and the newest commit waited for
// the next push.
func TestATriggerDuringAStartIsBuilt(t *testing.T) {
	// The build appends a mark and sleeps, so a trigger has time to land
	// mid-build; each pass leaves one mark.
	bare := makeRepo(t, "#!/bin/sh\necho mark >> \"$MIDDLEWARE_SITE_DIR/marks\"\nsleep 0.3\n")
	m := &manifest.Manifest{Version: 2, Deploy: &manifest.Deploy{GitHub: &manifest.GitHubDeploy{Repository: "acme/app", Build: [][]string{{"./build.sh"}}}}}
	root, dir := makeSite(t, m)
	r, _ := newRunner(root, bare, nil)
	// A first GitHub deploy puts the repository (and build.sh) in place so
	// the operator's in-place Deploy has something to build.
	if _, err := r.Run(context.Background(), "app.test", dir, m, "test", FromGitHub); err != nil {
		t.Fatal(err)
	}
	// The operator presses Deploy (in place); Start claims the site and
	// returns at once. A push arrives before the build finishes.
	if _, err := r.Start("app.test", dir, m, "operator", InPlace); err != nil {
		t.Fatal(err)
	}
	r.Trigger("app.test", dir, m, "github:someone")
	deadline := time.Now().Add(10 * time.Second)
	for r.Deploying("app.test") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if r.Deploying("app.test") {
		t.Fatal("the deployment never finished")
	}
	marks, _ := os.ReadFile(filepath.Join(dir, "marks"))
	// First deploy, the Start, and the triggered run: three marks.
	if got := strings.Count(string(marks), "mark"); got != 3 {
		t.Fatalf("the trigger during Start was not built: %d builds, want 3\n%s", got, marks)
	}
}

// A build step that outlives its timeout is killed with everything it
// started, not only the shell: npm's children used to keep running.
func TestATimedOutStepIsKilledWithItsChildren(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sites", "t.test")
	os.MkdirAll(dir, 0o755)
	g := &manifest.GitHubDeploy{Build: [][]string{{"sh", "-c", "sleep 30 & echo $! > child.pid; wait"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var log strings.Builder
	if _, err := Build(ctx, "t.test", dir, &manifest.Manifest{Version: 2}, g, &log, BuildOptions{BuildID: "b1", Home: filepath.Join(root, "home")}); err == nil {
		t.Fatal("a timed-out step succeeded")
	}
	time.Sleep(300 * time.Millisecond)
	data, _ := os.ReadFile(filepath.Join(dir, "child.pid"))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if pid > 0 && syscall.Kill(pid, 0) == nil && !zombie(pid) {
		stat, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the step's child survived the timeout: %s\n%s", stat, log.String())
	}
}

// zombie reports whether pid is dead but not yet reaped. The killed child
// is orphaned to PID 1, and where PID 1 does not reap (a container whose
// init is a plain process) it stays a zombie that kill(pid, 0) still
// finds: dead all the same, and the test used to fail on such machines.
// A pid /proc no longer knows is dead too.
func zombie(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	i := bytes.LastIndexByte(data, ')')
	return i < 0 || i+2 >= len(data) || data[i+2] == 'Z'
}

// A deploy checks out attacker-controlled files, so a hook planted in
// .git/hooks must not run on the host: core.hooksPath is nulled. The hook
// here would drop a marker; the deploy must finish clean with no marker.
func TestADeployDoesNotRunAPlantedGitHook(t *testing.T) {
	bare := makeRepo(t, "#!/bin/sh\ntrue\n")
	m := &manifest.Manifest{Version: 2, Deploy: &manifest.Deploy{GitHub: &manifest.GitHubDeploy{Repository: "acme/app", Build: [][]string{{"./build.sh"}}}}}
	root, dir := makeSite(t, m)
	r, _ := newRunner(root, bare, nil)
	// First deploy brings the .git into place.
	if _, err := r.Run(context.Background(), "app.test", dir, m, "test", FromGitHub); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "ran")
	hook := filepath.Join(dir, ".git", "hooks", "post-checkout")
	os.MkdirAll(filepath.Dir(hook), 0o755)
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := r.Run(context.Background(), "app.test", dir, m, "test", FromGitHub)
	if err != nil || st.Status != "ok" {
		t.Fatalf("second deploy: %+v %v", st, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the planted post-checkout hook ran during the deploy")
	}
}

// A command-bearing key in .git/config (a compromised container can write
// it; it is not tracked and so cannot arrive by push) must stop the deploy
// before git runs it, and leave the running build untouched.
func TestADeployRefusesACommandBearingGitConfig(t *testing.T) {
	bare := makeRepo(t, "#!/bin/sh\ntrue\n")
	m := &manifest.Manifest{Version: 2, Deploy: &manifest.Deploy{GitHub: &manifest.GitHubDeploy{Repository: "acme/app", Build: [][]string{{"./build.sh"}}, Pointer: ".current"}}}
	root, dir := makeSite(t, m)
	r, _ := newRunner(root, bare, nil)
	if _, err := r.Run(context.Background(), "app.test", dir, m, "test", FromGitHub); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, ".current"), []byte("good\n"), 0o644)
	marker := filepath.Join(root, "smudged")
	// A filter smudge cannot be reached with -c (its driver name is the
	// attacker's); Guard must refuse the whole deploy.
	cfg := filepath.Join(dir, ".git", "config")
	data, _ := os.ReadFile(cfg)
	os.WriteFile(cfg, append(data, []byte("[filter \"evil\"]\n\tsmudge = touch "+marker+"\n")...), 0o644)
	st, err := r.Run(context.Background(), "app.test", dir, m, "test", FromGitHub)
	if err == nil || st.Status != "failed" {
		t.Fatalf("a deploy on an unsafe config must fail: %+v %v", st, err)
	}
	if !strings.Contains(err.Error(), "filter.evil.smudge") {
		t.Fatalf("the failure must name the key, got %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the filter smudge ran")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".current")); strings.TrimSpace(string(b)) != "good" {
		t.Fatalf("the pointer was touched: %q", b)
	}
}
