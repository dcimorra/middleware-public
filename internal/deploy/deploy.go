// Package deploy brings new code into a site folder and builds it while
// the current build keeps serving. A deployment is: fetch the branch,
// check it out, run the build commands, and only if every one of them
// succeeded write the pointer file the manifest's env_from_file reads.
// The engine sees the pointer change on its next tick and hands over; this
// package never touches a process. Without a pointer, it asks the engine
// for a restart through the hook the app plugs in.
package deploy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/manifest"
)

// Options configure a Runner.
type Options struct {
	// DataDir is data/: logs go to logs/<domain>/deploy.log and the last
	// status to deploys/<domain>.json.
	DataDir string
	Bus     *events.Bus
	// Restart is called after a successful build when the manifest has no
	// pointer: nothing else would make the engine notice. Nil means the
	// log says so and nothing restarts.
	Restart func(ctx context.Context, domain string) error
	// Token returns the token for a repository (empty for public ones).
	// It is called at deploy time so a rotated token is used without a
	// restart and so it lives nowhere but in memory for the one command.
	Token func(repository string) string
	// GitURL turns owner/name into a clone URL; defaults to github.com.
	GitURL func(repository string) string
	// Timeout bounds one deployment. Default one hour.
	Timeout time.Duration
}

// Status is the record of a site's last deployment, in
// data/deploys/<domain>.json.
type Status struct {
	Domain     string    `json:"domain"`
	Repository string    `json:"repository"`
	Branch     string    `json:"branch"`
	Commit     string    `json:"commit,omitempty"`
	BuildID    string    `json:"build_id"`
	Actor      string    `json:"actor"`
	Status     string    `json:"status"` // "running" | "ok" | "failed"
	Error      string    `json:"error,omitempty"`
	Started    time.Time `json:"started"`
	Finished   time.Time `json:"finished,omitempty"`
	Log        string    `json:"log"`
}

// Runner serialises deployments per site.
type Runner struct {
	opts  Options
	mu    sync.Mutex
	sites map[string]*siteState
}

type siteState struct {
	mu      sync.Mutex // held for the whole deployment
	running bool
	again   bool
}

// New builds a Runner.
func New(opts Options) *Runner {
	if opts.Bus == nil {
		opts.Bus = events.New("")
	}
	if opts.Timeout == 0 {
		opts.Timeout = time.Hour
	}
	if opts.GitURL == nil {
		opts.GitURL = func(repo string) string { return "https://github.com/" + repo + ".git" }
	}
	return &Runner{opts: opts, sites: map[string]*siteState{}}
}

// Match reports whether a push to repository on ref ("refs/heads/main")
// is one m wants. The branch defaults to main; the repository name is
// compared without case because GitHub preserves the owner's spelling
// and nobody types it the same twice.
func Match(m *manifest.Manifest, repository, ref string) bool {
	if m == nil || m.Deploy == nil || m.Deploy.GitHub == nil {
		return false
	}
	g := m.Deploy.GitHub
	if !strings.EqualFold(strings.TrimSuffix(g.Repository, ".git"), strings.TrimSuffix(repository, ".git")) {
		return false
	}
	return ref == "refs/heads/"+Branch(g)
}

// Branch is the configured branch or main.
func Branch(g *manifest.GitHubDeploy) string {
	if g.Branch == "" {
		return "main"
	}
	return g.Branch
}

func (r *Runner) state(domain string) *siteState {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sites[domain]
	if !ok {
		s = &siteState{}
		r.sites[domain] = s
	}
	return s
}

// Run deploys one site and blocks until it is done. Deployments of the
// same site run one at a time; different sites run in parallel.
func (r *Runner) Run(ctx context.Context, domain, siteDir string, m *manifest.Manifest, actor string) (Status, error) {
	if m == nil || m.Deploy == nil || m.Deploy.GitHub == nil {
		return Status{}, fmt.Errorf("%s: no deploy.github in the manifest", domain)
	}
	s := r.state(domain)
	s.mu.Lock()
	defer s.mu.Unlock()
	return r.run(ctx, domain, siteDir, m.Deploy.GitHub, actor)
}

// Trigger starts a deployment in the background and returns at once. A
// trigger that arrives while the site is deploying marks it to run once
// more when done: two pushes in a row end with the newest commit built,
// and ten pushes in a row do not queue ten builds.
func (r *Runner) Trigger(domain, siteDir string, m *manifest.Manifest, actor string) {
	if m == nil || m.Deploy == nil || m.Deploy.GitHub == nil {
		return
	}
	s := r.state(domain)
	r.mu.Lock()
	if s.running {
		s.again = true
		r.mu.Unlock()
		return
	}
	s.running = true
	r.mu.Unlock()
	g := *m.Deploy.GitHub
	go func() {
		for {
			s.mu.Lock()
			_, _ = r.run(context.Background(), domain, siteDir, &g, actor)
			s.mu.Unlock()
			r.mu.Lock()
			if !s.again {
				s.running = false
				r.mu.Unlock()
				return
			}
			s.again = false
			r.mu.Unlock()
		}
	}()
}

// Last returns the last recorded status of a site.
func (r *Runner) Last(domain string) (Status, error) {
	var st Status
	err := fsx.ReadJSON(r.statusPath(domain), &st)
	return st, err
}

// All returns every recorded status, by domain.
func (r *Runner) All() []Status {
	entries, _ := os.ReadDir(filepath.Join(r.opts.DataDir, "deploys"))
	var out []Status
	for _, e := range entries {
		var st Status
		if fsx.ReadJSON(filepath.Join(r.opts.DataDir, "deploys", e.Name()), &st) == nil {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}

func (r *Runner) statusPath(domain string) string {
	return filepath.Join(r.opts.DataDir, "deploys", domain+".json")
}

func (r *Runner) save(st Status) {
	_ = fsx.WriteJSON(r.statusPath(st.Domain), st, 0o644)
}

// run is one deployment, with the site's lock held by the caller.
func (r *Runner) run(ctx context.Context, domain, siteDir string, g *manifest.GitHubDeploy, actor string) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	logPath := filepath.Join(r.opts.DataDir, "logs", domain, "deploy.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return Status{}, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return Status{}, err
	}
	defer logFile.Close()
	st := Status{Domain: domain, Repository: g.Repository, Branch: Branch(g), BuildID: time.Now().UTC().Format("20060102-150405"), Actor: actor, Status: "running", Started: time.Now(), Log: logPath}
	r.save(st)
	r.opts.Bus.Emit(events.Event{Type: "deploy.started", Subject: domain, Actor: actor, Data: map[string]any{"repository": st.Repository, "branch": st.Branch, "build_id": st.BuildID}})
	fmt.Fprintf(logFile, "\n=== %s deploy %s: %s@%s (by %s)\n", st.Started.Format(time.RFC3339), st.BuildID, st.Repository, st.Branch, actor)

	token := ""
	if r.opts.Token != nil {
		token = r.opts.Token(g.Repository)
	}
	res, err := Run(ctx, siteDir, g, logFile, RunOptions{
		Domain:  domain,
		BuildID: st.BuildID,
		GitURL:  r.opts.GitURL(g.Repository),
		Token:   token,
		Home:    filepath.Join(r.opts.DataDir, "homes", domain),
	})
	st.Commit = res.Commit
	st.Finished = time.Now()
	if err != nil {
		st.Status, st.Error = "failed", err.Error()
		fmt.Fprintf(logFile, "=== failed: %v\n", err)
		r.save(st)
		r.opts.Bus.Emit(events.Event{Type: "deploy.failed", Subject: domain, Actor: actor, Level: "error", Message: err.Error(), Data: map[string]any{"build_id": st.BuildID, "commit": st.Commit, "log": logPath}})
		return st, err
	}
	switch {
	case res.PointerWritten:
		fmt.Fprintf(logFile, "=== pointer %s now says %s; the engine hands over on its next tick\n", g.Pointer, st.BuildID)
	case r.opts.Restart != nil:
		if err := r.opts.Restart(ctx, domain); err != nil {
			fmt.Fprintf(logFile, "=== built, but the restart failed: %v\n", err)
		} else {
			fmt.Fprintln(logFile, "=== no pointer: asked the engine to restart the site")
		}
	default:
		fmt.Fprintln(logFile, "=== no pointer and no restart hook: the running instance keeps serving the previous build")
	}
	st.Status = "ok"
	fmt.Fprintf(logFile, "=== done in %s\n", st.Finished.Sub(st.Started).Round(time.Millisecond))
	r.save(st)
	r.opts.Bus.Emit(events.Event{Type: "deploy.done", Subject: domain, Actor: actor, Data: map[string]any{"build_id": st.BuildID, "commit": st.Commit, "pointer": res.PointerWritten, "duration": st.Finished.Sub(st.Started).String()}})
	return st, nil
}

// RunOptions parametrise one Run.
type RunOptions struct {
	Domain  string
	BuildID string
	GitURL  string
	// Token, when set, authenticates the fetch as x-access-token without
	// ever reaching .git/config: it travels in a header set on the one
	// command line, and the remote URL stays the public one.
	Token string
	// Home is the HOME the build and git run with, the site's own.
	Home string
}

// Result is what Run did.
type Result struct {
	Commit         string
	PointerWritten bool
}

// Run is one deployment into siteDir: fetch, checkout, build, pointer.
// log receives everything git and the build commands print. It is the
// low-level step; Runner adds the per-site lock, the status file and the
// events.
//
// The build commands see the manifest's resolved environment plus
// BUILD_ID, DEPLOY_COMMIT, MIDDLEWARE_DOMAIN and MIDDLEWARE_SITE_DIR, and
// every env_from_file variable that reads the pointer is set to the new
// build id, so a `next build` that reads NEXT_DIST_DIR from the
// environment writes into the directory the pointer is about to name.
// $BUILD_ID, $DOMAIN and $SITE_DIR expand inside the commands.
func Run(ctx context.Context, siteDir string, g *manifest.GitHubDeploy, log io.Writer, o RunOptions) (Result, error) {
	var res Result
	if g == nil || !strings.Contains(g.Repository, "/") {
		return res, errors.New("deploy.github.repository: want owner/name")
	}
	if o.BuildID == "" {
		o.BuildID = time.Now().UTC().Format("20060102-150405")
	}
	if o.GitURL == "" {
		o.GitURL = "https://github.com/" + g.Repository + ".git"
	}
	if o.Home == "" {
		o.Home = siteDir
	}
	if err := os.MkdirAll(o.Home, 0o700); err != nil {
		return res, err
	}
	branch := Branch(g)
	git := func(args ...string) error {
		return runGit(ctx, siteDir, o, log, args...)
	}
	if !fsx.Exists(filepath.Join(siteDir, ".git")) {
		// The folder already holds site.json, so `git clone` into it is
		// refused; init + fetch + checkout gets the same result and keeps
		// the manifest. If the repository ships its own site.json, the
		// checkout below overwrites the local one and the log says so.
		if err := git("init", "-q"); err != nil {
			return res, err
		}
		if err := git("remote", "add", "origin", o.GitURL); err != nil {
			return res, err
		}
	} else if err := git("remote", "set-url", "origin", o.GitURL); err != nil {
		if err := git("remote", "add", "origin", o.GitURL); err != nil {
			return res, err
		}
	}
	if err := git("fetch", "--prune", "--no-tags", "origin", "+refs/heads/"+branch+":refs/remotes/origin/"+branch); err != nil {
		return res, fmt.Errorf("fetch %s: %w", branch, err)
	}
	manifestBefore, _ := os.ReadFile(filepath.Join(siteDir, manifest.FileName))
	if err := git("checkout", "-q", "-f", "-B", branch, "origin/"+branch); err != nil {
		return res, fmt.Errorf("checkout %s: %w", branch, err)
	}
	if manifestAfter, _ := os.ReadFile(filepath.Join(siteDir, manifest.FileName)); len(manifestBefore) > 0 && string(manifestAfter) != string(manifestBefore) {
		fmt.Fprintln(log, "note: the repository ships its own site.json and it replaced the local one")
	}
	commit, err := gitOutput(ctx, siteDir, o, "rev-parse", "HEAD")
	if err != nil {
		return res, err
	}
	res.Commit = strings.TrimSpace(commit)
	fmt.Fprintf(log, "at %s\n", res.Commit)

	env, err := buildEnv(siteDir, g, o, res.Commit)
	if err != nil {
		return res, err
	}
	vars := map[string]string{"BUILD_ID": o.BuildID, "DOMAIN": o.Domain, "SITE_DIR": siteDir}
	for i, raw := range g.Build {
		if len(raw) == 0 {
			return res, fmt.Errorf("build[%d]: empty command", i)
		}
		argv := make([]string, len(raw))
		for j, a := range raw {
			argv[j] = manifest.Expand(a, vars)
		}
		fmt.Fprintf(log, "$ %s\n", strings.Join(argv, " "))
		bin := argv[0]
		if strings.HasPrefix(bin, "./") || strings.HasPrefix(bin, "../") {
			bin = filepath.Join(siteDir, bin)
		}
		cmd := exec.CommandContext(ctx, bin, argv[1:]...)
		cmd.Dir = siteDir
		cmd.Env = env
		cmd.Stdout, cmd.Stderr = log, log
		cmd.Stdin = nil
		// A build that leaves a daemon behind holding our log would hang
		// Wait forever; after the command itself exits, the copy is cut.
		cmd.WaitDelay = 5 * time.Second
		if err := cmd.Run(); err != nil {
			return res, fmt.Errorf("build[%d] %s: %w", i, argv[0], err)
		}
	}
	if g.Pointer != "" {
		p := filepath.Join(siteDir, g.Pointer)
		if !strings.HasPrefix(filepath.Clean(p), filepath.Clean(siteDir)+string(filepath.Separator)) {
			return res, fmt.Errorf("pointer %q leaves the site folder", g.Pointer)
		}
		if err := fsx.WriteAtomic(p, []byte(o.BuildID+"\n"), 0o644); err != nil {
			return res, fmt.Errorf("pointer: %w", err)
		}
		res.PointerWritten = true
	}
	return res, nil
}

// buildEnv is the site's resolved environment (when it has a run section)
// with the deploy variables on top. HOME is the site's own, PATH the
// supervisor's, nothing else leaks from the supervisor.
func buildEnv(siteDir string, g *manifest.GitHubDeploy, o RunOptions, commit string) ([]string, error) {
	env := map[string]string{}
	for _, k := range []string{"PATH", "LANG", "LC_ALL", "TZ"} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	m, err := manifest.Load(siteDir)
	if err == nil && m.Run != nil {
		spec, err := manifest.Resolve(o.Domain, siteDir, m)
		if err == nil {
			for k, v := range spec.Env {
				env[k] = v
			}
			for k, file := range m.Run.EnvFromFile {
				if g.Pointer != "" && filepath.Clean(file) == filepath.Clean(g.Pointer) {
					env[k] = o.BuildID
				}
			}
		}
	}
	env["HOME"] = o.Home
	env["MIDDLEWARE_DOMAIN"] = o.Domain
	env["MIDDLEWARE_SITE_DIR"] = siteDir
	env["BUILD_ID"] = o.BuildID
	env["DEPLOY_COMMIT"] = commit
	env["CI"] = "true"
	delete(env, "PORT")
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out, nil
}

func gitEnv(o RunOptions) []string {
	env := []string{"HOME=" + o.Home, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	if p, ok := os.LookupEnv("PATH"); ok {
		env = append(env, "PATH="+p)
	}
	return env
}

// gitArgs prefixes the auth header for the one command when a token is
// set. The header is what GitHub's own checkout action sends; a URL with
// the token in it would be saved by `remote add` into .git/config.
func gitArgs(o RunOptions, args []string) []string {
	if o.Token == "" {
		return args
	}
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + o.Token))
	return append([]string{"-c", "http.extraheader=AUTHORIZATION: basic " + auth}, args...)
}

func runGit(ctx context.Context, dir string, o RunOptions, log io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", gitArgs(o, args)...)
	cmd.Dir = dir
	cmd.Env = gitEnv(o)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.WaitDelay = 5 * time.Second
	fmt.Fprintf(log, "$ git %s\n", strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, o RunOptions, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", gitArgs(o, args)...)
	cmd.Dir = dir
	cmd.Env = gitEnv(o)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return string(out), nil
}
