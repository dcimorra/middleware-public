package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent/agy"
	"github.com/dcimorra/the-middleware/internal/ptyx"
)

// Signing in with a subscription, from the panel.
//
// The problem: a subscription session is only obtained by an interactive
// program. `claude setup-token` paints with escape sequences and waits
// for a pasted code; the Antigravity CLI offers a menu of sign-in
// methods, then prints a Google URL and asks for the code at a prompt. Both have to run on the
// machine where the session will be used, which on a server reached from
// a browser leaves SSH as the only way, and the operator administering
// the house from a phone has no SSH at hand. From the chat it cannot be
// done either: a turn is `-p`, with no slash commands and no keyboard.
//
// Three decisions that do not show:
//
//   - A real terminal, not a pipe. Without one neither program asks for
//     anything and both sit waiting for a keyboard nobody has.
//   - A temporary HOME, which is the line that keeps this from breaking
//     anything: both programs write the session under HOME, so a failed
//     attempt on the real home would leave the house without the session
//     it had. What is kept is the secret that comes out, in the module's
//     encrypted store, and the folder is deleted whole.
//   - The URL is read from the hyperlink (OSC 8), not from the text, when
//     there is one: a TUI paints the address in pieces and moves the
//     cursor between them, so what is on screen cannot be joined back;
//     the escape sequence carries the whole address. A program that
//     prints it plainly is read with a plain https match.
//
// The lessons that cost in the first version are kept as they were
// learned: a rejected code is not a failure (the program offers to retry
// and waits for a key), a long code needs a pause before its Enter, and
// a TUI repaints, so only what has just arrived is looked at.

// Login is one attempt in flight.
type Login struct {
	spec  loginSpec
	mu    sync.Mutex
	pty   *ptyx.Pty
	cmd   *exec.Cmd
	home  string
	buf   []byte
	state string
	url   string
	// secret is what the module stores: the long-lived token for Claude,
	// the token file for Antigravity.
	secret string
	fail   string
	// userCode is a device sign-in's code, shown beside the address.
	userCode string
	// stale is the address a rejected code came from; the next one must
	// differ from it or the panel would show an address nobody waits on.
	stale string
	// retries counts rejected codes; SendCode's watcher compares it.
	retries int
	// chose says the sign-in method was picked from the program's menu.
	chose bool
	// scanned is how much of the buffer has been looked at for things
	// that happen once.
	scanned int
	done    chan struct{}
	started time.Time
}

// loginSpec is what differs between the two CLIs.
type loginSpec struct {
	provider string
	args     []string
	env      []string
	// codePrompt says the program is ready for the code; Claude says it
	// alongside the address, Antigravity after it.
	codePrompt *regexp.Regexp
	// retry is what the program prints when the code was not good.
	retry *regexp.Regexp
	// fromOutput finds the secret in what was printed, if it is printed.
	fromOutput func(buf []byte) string
	// fromDisk finds it in the temporary HOME, for programs that write it.
	fromDisk func(home string) string
	// exitsWhenDone: Claude exits after printing the token; Antigravity goes on
	// into its TUI and has to be stopped once the credentials exist.
	exitsWhenDone bool
	// prepareHome seeds the temporary HOME before the program starts.
	prepareHome func(home string) error
	// menu is a choice the program asks for before it prints an address;
	// its first option, the account sign-in, is taken with an Enter.
	menu *regexp.Regexp
	// urlOK says an address printed is the one to authorize at; nil is
	// an OAuth address.
	urlOK func(u string) bool
	// device, for a device-code sign-in (Codex): the code the person types
	// on the vendor's page. Nothing is pasted back; the program finishes
	// by itself once the code is entered there.
	device *regexp.Regexp
}

// The states the panel sees. In this order, and never back, except
// authorize → starting after a rejected code (a new address is printed).
const (
	LoginStarting  = "starting"
	LoginAuthorize = "authorize"
	LoginChecking  = "checking"
	LoginDone      = "done"
	LoginFailed    = "failed"
)

// loginIdle is how long a half-finished login is kept. Generous, because
// the slow part is a person walking to another device to open a link.
const loginIdle = 15 * time.Minute

// checkingMax is how long the program gets to say something about a
// code that was typed in.
var checkingMax = 2 * time.Minute

// pastePause is the gap between the code and the Enter that submits it.
// A real code is a hundred-odd characters; sent in one write with the
// newline the TUI takes it all as pasted text and the Enter is not an
// Enter. With a short code it does not happen, which is why the first
// tests said this worked.
var pastePause = 250 * time.Millisecond

var (
	osc8      = regexp.MustCompile(`\x1b]8;[^;]*;(https://[^\x07\x1b]+)`)
	plainURL  = regexp.MustCompile(`https://[^\s\x07\x1b"'<>]+`)
	oatToken  = regexp.MustCompile(`sk-ant-oat[0-9]*-[A-Za-z0-9_-]{20,}`)
	statusRe  = regexp.MustCompile(`status code (\d{3})`)
	claudeOut = regexp.MustCompile(`Paste code here`)
	claudeRe  = regexp.MustCompile(`Press Enter to retry|to retry\.`)
	agyMenu   = regexp.MustCompile(`Select login method`)
	agyOut    = regexp.MustCompile(`paste the authorization code`)
	agyRe     = regexp.MustCompile(`failed to exchange authorization code|invalid_grant|Failed to sign in`)
	codexCode = regexp.MustCompile(`one-time code[^\n]*\n\s*([A-Z0-9]{4}-[A-Z0-9]{4,6})`)
)

// codexLoginSpec is ChatGPT's device-code sign-in: `codex login
// --device-auth` prints an address and a code, polls until the code is
// entered at the address, writes auth.json and exits.
func codexLoginSpec(bin string) loginSpec {
	return loginSpec{
		provider:   "openai",
		args:       []string{bin, "login", "--device-auth"},
		fromOutput: func([]byte) string { return "" },
		fromDisk: func(home string) string {
			raw, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
			if err != nil || !strings.Contains(string(raw), "refresh_token") {
				return ""
			}
			return string(raw)
		},
		exitsWhenDone: true,
		urlOK:         func(u string) bool { return strings.Contains(u, "auth.openai.com") },
		device:        codexCode,
		// Codex refuses a CODEX_HOME that does not exist.
		prepareHome: func(home string) error { return os.MkdirAll(filepath.Join(home, ".codex"), 0o700) },
		env:         []string{"CODEX_HOME={home}/.codex"},
	}
}

func claudeLoginSpec(bin string) loginSpec {
	return loginSpec{
		provider:   "claude",
		args:       []string{bin, "setup-token"},
		codePrompt: claudeOut,
		retry:      claudeRe,
		fromOutput: func(buf []byte) string {
			if m := oatToken.Find(buf); m != nil {
				return string(m)
			}
			return ""
		},
		fromDisk:      claudeTokenFromDisk,
		exitsWhenDone: true,
	}
}

// agyLoginSpec is the Antigravity CLI's Google sign-in: run bare with no
// session, it asks how to sign in (the first option is the Google
// account), prints the address, waits for the code and writes the token
// file, then goes on into its TUI.
func agyLoginSpec(bin string) loginSpec {
	return loginSpec{
		provider:   "agy",
		args:       []string{bin},
		env:        []string{"AGY_CLI_DISABLE_AUTO_UPDATE=1", "NO_BROWSER=true"},
		menu:       agyMenu,
		codePrompt: agyOut,
		retry:      agyRe,
		fromOutput: func([]byte) string { return "" },
		fromDisk:   agyTokenFromDisk,
		urlOK:      func(u string) bool { return strings.HasPrefix(u, "https://accounts.google.com/") },
	}
}

// claudeTokenFromDisk digs the token out of the credentials file, for
// the day setup-token stops printing it.
func claudeTokenFromDisk(home string) string {
	raw, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return ""
	}
	if m := oatToken.Find(raw); m != nil {
		return string(m)
	}
	var any any
	if json.Unmarshal(raw, &any) != nil {
		return ""
	}
	return findAccessToken(any)
}

// agyTokenFromDisk returns the token file the Antigravity CLI wrote,
// whole: it holds the refresh token, and the CLI reads it back from the
// same place under whatever HOME a turn runs with.
func agyTokenFromDisk(home string) string {
	raw, err := os.ReadFile(filepath.Join(agy.Dir(home), agy.TokenFile))
	if err != nil || !agy.HasSignIn(string(raw)) {
		return ""
	}
	return string(raw)
}

func findAccessToken(v any) string {
	switch t := v.(type) {
	case map[string]any:
		for k, sub := range t {
			if strings.EqualFold(k, "accessToken") || strings.EqualFold(k, "access_token") {
				if s, ok := sub.(string); ok && s != "" {
					return s
				}
			}
			if s := findAccessToken(sub); s != "" {
				return s
			}
		}
	case []any:
		for _, sub := range t {
			if s := findAccessToken(sub); s != "" {
				return s
			}
		}
	}
	return ""
}

// startLogin runs the program on a terminal of its own and waits a few
// seconds for the address. path is the resolved binary; extraPath is
// prepended to PATH so a managed Node is found by the CLI's shebang.
func startLogin(spec loginSpec, path, extraPath string) (*Login, error) {
	sweepLogins()
	home, err := os.MkdirTemp("", "middleware-login-")
	if err != nil {
		return nil, err
	}
	if spec.prepareHome != nil {
		if err := spec.prepareHome(home); err != nil {
			os.RemoveAll(home)
			return nil, err
		}
	}
	pt, err := ptyx.Open()
	if err != nil {
		os.RemoveAll(home)
		return nil, err
	}
	// Wide enough that the address is not wrapped into more pieces than
	// it has to be; narrow terminals make the rest unreadable too.
	_ = pt.Resize(40, 160)
	args := append([]string(nil), spec.args[1:]...)
	cmd := exec.Command(path, args...)
	cmd.Dir = home
	p := os.Getenv("PATH")
	if extraPath != "" {
		p = extraPath + string(os.PathListSeparator) + p
	}
	// A clean environment but for what a program needs to exist: no API
	// key (with one set, neither CLI has a reason to ask for a session)
	// and no HOME but this one.
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + p,
		"TERM=xterm-256color",
		"LANG=C.UTF-8",
		// No browser to open: this is a server.
		"BROWSER=true",
	}
	for _, e := range spec.env {
		cmd.Env = append(cmd.Env, strings.ReplaceAll(e, "{home}", home))
	}
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS"} {
		if v := os.Getenv(k); v != "" {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	if err := pt.Start(cmd); err != nil {
		pt.Close()
		os.RemoveAll(home)
		return nil, err
	}
	l := &Login{spec: spec, pty: pt, cmd: cmd, home: home, state: LoginStarting, done: make(chan struct{}), started: time.Now()}
	go l.read()
	go l.reap()
	go func() {
		select {
		case <-l.done:
		case <-time.After(loginIdle):
			if l.Live() {
				l.Cancel()
			}
		}
	}()
	// A few seconds for the address, which usually takes three or four
	// and saves the panel a round trip; not more, because a long wait
	// inside a request is the known way to lose the answer. What does
	// not arrive in time, the poll brings.
	l.waitFor(func(l *Login) bool { return l.url != "" || l.state == LoginFailed }, 12*time.Second)
	go l.watchStarting()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == LoginFailed {
		return l, errors.New(l.fail)
	}
	return l, nil
}

func (l *Login) watchStarting() {
	l.waitFor(func(l *Login) bool { return l.state != LoginStarting }, time.Minute)
	l.mu.Lock()
	if l.state != LoginStarting {
		l.mu.Unlock()
		return
	}
	l.state = LoginFailed
	l.fail = "the program did not say where to authorize"
	l.mu.Unlock()
	l.Cancel()
}

// read drains the terminal. Everything is kept (a few kilobytes, and
// what gets shown when this goes wrong) and parsed as it arrives.
func (l *Login) read() {
	buf := make([]byte, 4096)
	for {
		n, err := l.pty.Read(buf)
		if n > 0 {
			l.mu.Lock()
			l.buf = append(l.buf, buf[:n]...)
			if len(l.buf) > 1<<20 {
				cut := len(l.buf) - (1 << 19)
				l.buf = l.buf[cut:]
				l.scanned = max(0, l.scanned-cut)
			}
			l.parseLocked()
			l.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// parseLocked reads out of what has been printed the things worth
// knowing: the address, the prompt, the secret, a rejection.
func (l *Login) parseLocked() {
	if l.spec.menu != nil && !l.chose && l.state == LoginStarting && l.spec.menu.Match(l.buf) {
		l.chose = true
		go l.press()
	}
	// The last address, not the first: a rejected code makes the program
	// start over with a new challenge.
	var urls []string
	for _, m := range osc8.FindAllSubmatch(l.buf, -1) {
		urls = append(urls, string(m[1]))
	}
	if len(urls) == 0 {
		for _, m := range plainURL.FindAll(l.buf, -1) {
			u := string(m)
			// A TUI wraps a plain address over several lines; the longest
			// match that starts at the same prefix is the whole one, and
			// a fragment is a prefix of it.
			if n := len(urls); n > 0 && strings.HasPrefix(u, urls[n-1]) {
				urls[n-1] = u
				continue
			}
			urls = append(urls, u)
		}
	}
	if n := len(urls); n > 0 {
		u := urls[n-1]
		ok := strings.Contains(u, "oauth")
		if l.spec.urlOK != nil {
			ok = l.spec.urlOK(u)
		}
		if l.spec.device != nil {
			plain := ansi.ReplaceAll(l.buf, nil)
			if m := l.spec.device.FindAllSubmatch(plain, -1); len(m) > 0 {
				l.userCode = string(m[len(m)-1][1])
			}
			ok = ok && l.userCode != ""
		}
		if u != l.stale && ok {
			l.url = u
			if l.state == LoginStarting {
				l.state = LoginAuthorize
			}
		}
	}
	if l.secret == "" {
		if s := l.spec.fromOutput(l.buf); s != "" {
			l.secret = s
			l.state = LoginDone
			return
		}
		if l.state == LoginChecking {
			if s := l.spec.fromDisk(l.home); s != "" {
				l.secret = s
				l.state = LoginDone
				if !l.spec.exitsWhenDone {
					// The credentials are on disk and the program would
					// now start its interactive screen; it is done.
					go l.kill()
				}
				return
			}
		}
	}
	// A code it did not like. Only in what has just arrived, because a
	// TUI repaints and the line stays in the buffer for ever; and only
	// while a code is being checked.
	fresh := l.buf[min(l.scanned, len(l.buf)):]
	if l.state == LoginChecking && l.spec.retry != nil && l.spec.retry.Match(fresh) {
		l.retries++
		l.fail = "that code was not accepted"
		if m := statusRe.FindSubmatch(fresh); m != nil {
			l.fail += " (" + string(m[1]) + ")"
		}
		l.fail += ". Authorize again with the link, which is a new one, and paste the new code."
		l.stale = l.url
		l.url = ""
		l.state = LoginStarting
		if l.spec.exitsWhenDone {
			// Claude waits for Enter to ask again.
			go l.press()
		} else {
			// Antigravity exits on a bad code; the next attempt is a new run.
			l.state = LoginFailed
		}
	}
	l.scanned = len(l.buf)
}

func (l *Login) press() {
	if l.pty != nil {
		_, _ = l.pty.Write([]byte("\r"))
	}
}

func (l *Login) kill() {
	if l.cmd.Process != nil {
		_ = l.cmd.Process.Kill()
	}
}

// reap waits for the program and settles what happened.
func (l *Login) reap() {
	defer close(l.done)
	err := l.cmd.Wait()
	// The reader is on the other end of the same terminal: give it the
	// moment it takes to pick up what was printed last, which is where
	// the token is.
	time.Sleep(300 * time.Millisecond)
	l.mu.Lock()
	l.parseLocked()
	if l.secret == "" {
		l.secret = l.spec.fromDisk(l.home)
		if l.secret != "" {
			l.state = LoginDone
		}
	}
	if l.state != LoginDone {
		l.state = LoginFailed
		if l.fail == "" {
			l.fail = loginError(l.buf, err)
		}
	}
	l.mu.Unlock()
	l.pty.Close()
	// The session lives in the secret from here on; what is left in the
	// folder is a copy in somebody else's format.
	os.RemoveAll(l.home)
}

// SendCode pastes the code the browser gave back.
func (l *Login) SendCode(code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return errors.New("no code was pasted")
	}
	if strings.ContainsAny(code, "\r\n") {
		return errors.New("the code has a line break inside")
	}
	l.mu.Lock()
	if l.state != LoginAuthorize {
		st := l.state
		l.mu.Unlock()
		return fmt.Errorf("not the moment to paste a code (%s)", st)
	}
	l.state = LoginChecking
	l.fail = ""
	tries := l.retries
	l.mu.Unlock()
	if _, err := l.pty.Write([]byte(code)); err != nil {
		return err
	}
	time.Sleep(pastePause)
	if _, err := l.pty.Write([]byte("\r")); err != nil {
		return err
	}
	// Answer now, without waiting for the outcome: waiting here was a
	// request of up to two minutes, and anything that cuts a long wait
	// (the edge, a phone's network) took the answer with it. The poll
	// tells the end.
	go l.watchChecking(tries)
	return nil
}

func (l *Login) watchChecking(tries int) {
	l.waitFor(func(l *Login) bool { return l.state != LoginChecking || l.retries > tries }, checkingMax)
	l.mu.Lock()
	if l.state != LoginChecking {
		l.mu.Unlock()
		return
	}
	l.state = LoginFailed
	l.fail = "the program did not answer the code, for good or ill. What it had on screen is below"
	l.mu.Unlock()
	l.Cancel()
}

// Tail is the last few lines the program printed, escape sequences out
// and secrets blanked: the one thing that explains a login that neither
// accepts nor rejects.
func (l *Login) Tail(lines int) string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	raw := append([]byte(nil), l.buf...)
	l.mu.Unlock()
	raw = oatToken.ReplaceAll(raw, []byte("sk-ant-oat…"))
	var keep []string
	for _, line := range strings.Split(stripANSI(raw), "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if len(line) <= 2 {
			continue
		}
		keep = append(keep, line)
	}
	if len(keep) > lines {
		keep = keep[len(keep)-lines:]
	}
	return strings.Join(keep, "\n")
}

// Cancel tears down an attempt that is not going to finish.
func (l *Login) Cancel() {
	l.mu.Lock()
	if l.state != LoginDone {
		l.state = LoginFailed
		if l.fail == "" {
			l.fail = "cancelled"
		}
	}
	l.mu.Unlock()
	l.kill()
	<-l.done
}

// Live says whether this attempt is still worth talking to.
func (l *Login) Live() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state == LoginStarting || l.state == LoginAuthorize || l.state == LoginChecking
}

// UserCode is a device sign-in's code, once printed.
func (l *Login) UserCode() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.userCode
}

// State is what the panel reads.
func (l *Login) State() (state, url, fail string) {
	if l == nil {
		return "", "", ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state, l.url, l.fail
}

// Secret is what came out, once done.
func (l *Login) Secret() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != LoginDone {
		return ""
	}
	return l.secret
}

func (l *Login) waitFor(ok func(*Login) bool, limit time.Duration) {
	deadline := time.Now().Add(limit)
	for {
		l.mu.Lock()
		done := ok(l)
		l.mu.Unlock()
		if done || time.Now().After(deadline) {
			return
		}
		select {
		case <-l.done:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func loginError(raw []byte, err error) string {
	var keep []string
	for _, line := range strings.Split(stripANSI(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			keep = append(keep, line)
		}
	}
	if len(keep) > 4 {
		keep = keep[len(keep)-4:]
	}
	msg := strings.Join(keep, " · ")
	if msg == "" {
		if err != nil {
			return err.Error()
		}
		return "no session came out and the program did not say why"
	}
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	return msg
}

// ansi is every escape sequence this output uses. The CSI class carries
// `<=>?`: `ESC [ > 0 q` is one this TUI really sends, and a class of
// just digits leaves the escape in and the rest of the line out.
var ansi = regexp.MustCompile(
	`\x1b\][^\x07\x1b]*(\x07|\x1b\\)` +
		`|\x1b\[[0-9;:<=>?]*[ -/]*[@-~]` +
		`|\x1b[()][@-~]` +
		`|\x1b[@-Z\\-_]`)

var control = regexp.MustCompile(`[\x00-\x09\x0b-\x1f\x7f]`)

func stripANSI(b []byte) string {
	out := ansi.ReplaceAll(b, []byte(" "))
	return string(control.ReplaceAll(out, []byte(" ")))
}

// sweepLogins removes folders of logins that never finished: one is left
// each time the process dies with an attempt in flight. Only ours, only
// directories, only older than an hour.
func sweepLogins() {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "middleware-login-") {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < time.Hour {
			continue
		}
		_ = os.RemoveAll(filepath.Join(os.TempDir(), e.Name()))
	}
}
