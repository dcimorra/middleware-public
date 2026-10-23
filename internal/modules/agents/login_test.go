package agents

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What is fragile here is not talking to the vendor, the CLI does that;
// it is reading what a terminal program paints. That is testable with no
// account and no network: these are the real outputs, captured from
// `claude setup-token` 2.1.304 and `agy` 1.4.6 on a pty.

// claudeAsks is Claude Code's screen: the hyperlink carries the whole
// address while the visible text is cut at the column and continued on
// the next line inside another hyperlink to the same address.
const claudeAsks = "\x1b[2G\x1b[38;5;246mBrowser\x1b[10Gdidn't\x1b[17Gopen?\x1b[23GUse\x1b[27Gthe\x1b[31Gurl\x1b[35Gbelow\x1b[39m\r\r\n\r\r\n" +
	"\x1b]8;id=u8xrxd;https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&state=FIRST\x07" +
	"\x1b[38;5;246mhttps://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88\x1b[39m\x1b]8;;\x07\r\r\n" +
	"\x1b]8;id=u8xrxd;https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&state=FIRST\x07" +
	"\x1b[38;5;246med-5944d1962f5e&response_type=code&state=FIRST\x1b[39m\x1b]8;;\x07\r\r\n" +
	"\x1b[2GPaste\x1b[8Gcode\x1b[13Ghere\x1b[18Gif\x1b[21Gprompted\x1b[30G>\r\n"

// claudeRejects: a bad code does not fail, it offers to retry.
const claudeRejects = "\r  *****************bc#123\r\n" +
	"  \rOAuth error: Requ\x1b[3Dst failed with status code 400 \r  Press Enter to retry. \r"

// claudeSecond: the retry starts over with another challenge.
const claudeSecond = "\r  Retrying…\r\n" +
	"\x1b]8;id=1ljvoh6;https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a&state=SECOND\x07https://claude.com/cai/oauth\x1b]8;;\x07\r\n"

func feed(l *Login, s string) {
	l.mu.Lock()
	l.buf = append(l.buf, s...)
	l.parseLocked()
	l.mu.Unlock()
}

func newParseLogin(spec loginSpec) *Login {
	return &Login{spec: spec, state: LoginStarting, done: make(chan struct{}), home: os.TempDir()}
}

func TestClaudesAddressIsReadFromTheHyperlinkWhole(t *testing.T) {
	l := newParseLogin(claudeLoginSpec("claude"))
	feed(l, claudeAsks)
	state, url, _ := l.State()
	if state != LoginAuthorize {
		t.Fatalf("state %q, want authorize", state)
	}
	if !strings.HasSuffix(url, "state=FIRST") || strings.Contains(url, "\x1b") {
		t.Fatalf("address came out cut or dirty: %q", url)
	}
}

// The Antigravity CLI 1.4.6 on a pty, captured: first a menu of sign-in
// methods, which the login answers with an Enter, then the address in a
// hyperlink and the prompt for the code.
func TestAgysMenuIsAnsweredAndItsAddressReadWhole(t *testing.T) {
	menu, err := os.ReadFile(filepath.Join("testdata", "agy-menu-1.4.6.txt"))
	if err != nil {
		t.Fatal(err)
	}
	addr, err := os.ReadFile(filepath.Join("testdata", "agy-address-1.4.6.txt"))
	if err != nil {
		t.Fatal(err)
	}
	l := newParseLogin(agyLoginSpec("agy"))
	feed(l, string(menu))
	if !l.chose {
		t.Fatal("the sign-in method was not picked from the menu")
	}
	if state, _, _ := l.State(); state != LoginStarting {
		t.Fatalf("state %q before the address", state)
	}
	feed(l, string(addr))
	state, url, _ := l.State()
	if state != LoginAuthorize {
		t.Fatalf("state %q, want authorize", state)
	}
	if !strings.HasPrefix(url, "https://accounts.google.com/o/oauth2/auth?") || !strings.Contains(url, "redirect_uri=https%3A%2F%2Fantigravity.google%2Foauth-callback") || strings.ContainsAny(url, "\x1b\x07 ") {
		t.Fatalf("address: %q", url)
	}
}

func TestARejectedCodeGivesTheNewAddress(t *testing.T) {
	l := newParseLogin(claudeLoginSpec("claude"))
	l.pty = nil
	feed(l, claudeAsks)
	l.mu.Lock()
	l.state = LoginChecking
	l.mu.Unlock()
	feed(l, claudeRejects)
	state, url, fail := l.State()
	if state != LoginStarting || url != "" {
		t.Fatalf("after a rejection: state %q url %q", state, url)
	}
	if !strings.Contains(fail, "400") {
		t.Fatalf("the status code is the one part worth repeating: %q", fail)
	}
	// A repaint of the same rejection line must not count twice.
	feed(l, claudeRejects)
	if l.retries != 1 {
		t.Fatalf("a repaint counted as a new rejection: %d", l.retries)
	}
	feed(l, claudeSecond)
	state, url, _ = l.State()
	if state != LoginAuthorize || !strings.HasSuffix(url, "state=SECOND") {
		t.Fatalf("the second address was not taken: %q %q", state, url)
	}
}

func TestTheSecretIsReadFromTheOutputOrFromDisk(t *testing.T) {
	l := newParseLogin(claudeLoginSpec("claude"))
	feed(l, "Long-lived token:\r\n sk-ant-oat01-"+strings.Repeat("a", 40)+"\r\n")
	if state, _, _ := l.State(); state != LoginDone || !strings.HasPrefix(l.Secret(), "sk-ant-oat01-") {
		t.Fatalf("token not read from the output: %q %q", state, l.Secret())
	}
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-`+strings.Repeat("b", 40)+`"}}`), 0o600)
	if got := claudeTokenFromDisk(home); !strings.HasPrefix(got, "sk-ant-oat01-bbbb") {
		t.Fatalf("token not read from disk: %q", got)
	}
	ghome := t.TempDir()
	os.MkdirAll(filepath.Join(ghome, ".gemini", "antigravity-cli"), 0o700)
	os.WriteFile(filepath.Join(ghome, ".gemini", "antigravity-cli", "antigravity-oauth-token"), []byte(`{"token":{"access_token":"ya29.x","refresh_token":"1//r"},"auth_method":"consumer"}`), 0o600)
	if got := agyTokenFromDisk(ghome); !strings.Contains(got, "refresh_token") {
		t.Fatalf("antigravity sign-in not read from disk: %q", got)
	}
	if got := agyTokenFromDisk(t.TempDir()); got != "" {
		t.Fatalf("no file must be no secret, got %q", got)
	}
}

func TestTheTailNeverShowsTheToken(t *testing.T) {
	l := newParseLogin(claudeLoginSpec("claude"))
	feed(l, claudeAsks+"\r\nsk-ant-oat01-"+strings.Repeat("z", 40)+"\r\n")
	tail := l.Tail(10)
	if strings.Contains(tail, "zzzz") || !strings.Contains(tail, "sk-ant-oat…") {
		t.Fatalf("the token leaked into the tail: %q", tail)
	}
	if strings.Contains(tail, "\x1b") {
		t.Fatalf("escape sequences in the tail: %q", tail)
	}
}

func TestACodeWithALineBreakIsNotSent(t *testing.T) {
	l := newParseLogin(claudeLoginSpec("claude"))
	feed(l, claudeAsks)
	if err := l.SendCode("abc\ndef"); err == nil {
		t.Fatal("a newline inside the code is an Enter the program would act on")
	}
	if err := l.SendCode(""); err == nil {
		t.Fatal("an empty code is not a code")
	}
}

// TestALongCodeArrivesWholeAndThenItsEnter: a fake TUI on a real pty
// records what it reads; the code must come in one piece and the Enter
// in a later read, which is what the pause is for.
func TestALongCodeArrivesWholeAndThenItsEnter(t *testing.T) {
	script := filepath.Join(t.TempDir(), "tui.sh")
	os.WriteFile(script, []byte(`#!/bin/sh
printf 'Browser did not open? \033]8;;https://example.com/oauth/authorize?state=X\007link\033]8;;\007\n'
printf 'Paste code here if prompted > '
IFS= read -r code
printf '\nGOT:%s:END\n' "$code"
sleep 0.3
printf 'sk-ant-oat01-%s\n' "$(printf '%s' "$code" | tr -c 'A-Za-z0-9' 'x' | cut -c1-40)"
`), 0o755)
	spec := claudeLoginSpec(script)
	l, err := startLogin(spec, script, "")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Cancel()
	if state, url, _ := l.State(); state != LoginAuthorize || url == "" {
		t.Fatalf("no address from the fake: %q %q (%s)", state, url, l.Tail(5))
	}
	code := strings.Repeat("abcdefghij", 13) + "#1"
	if err := l.SendCode(code); err != nil {
		t.Fatal(err)
	}
	l.waitFor(func(l *Login) bool { return l.state == LoginDone || l.state == LoginFailed }, 10*time.Second)
	state, _, fail := l.State()
	if state != LoginDone {
		t.Fatalf("state %q (%s): %s", state, fail, l.Tail(6))
	}
	if !strings.Contains(string(l.buf), "GOT:"+code+":END") {
		t.Fatalf("the code did not arrive whole:\n%s", l.Tail(8))
	}
}

// TestSigningInForRealPrintsAnAddress runs the real CLIs up to the
// address, which needs no account: LOGIN=1, and the binary in PATH.
func TestSigningInForRealPrintsAnAddress(t *testing.T) {
	if os.Getenv("LOGIN") != "1" {
		t.Skip("set LOGIN=1 to run the real sign-in flows up to the address")
	}
	for _, c := range []struct {
		bin  string
		spec func(string) loginSpec
	}{{"claude", claudeLoginSpec}, {"agy", agyLoginSpec}} {
		path, err := exec.LookPath(c.bin)
		if err != nil {
			t.Logf("%s not in PATH, skipped", c.bin)
			continue
		}
		l, err := startLogin(c.spec(path), path, "")
		if err != nil {
			t.Fatalf("%s: %v", c.bin, err)
		}
		state, url, fail := l.State()
		l.Cancel()
		if state != LoginAuthorize || !strings.Contains(url, "oauth") {
			t.Fatalf("%s: state %q url %q fail %q", c.bin, state, url, fail)
		}
		t.Logf("%s asks to authorize at %.60s…", c.bin, url)
	}
	_ = context.Background
}

// What `codex login --device-auth` 0.163 prints: the address has no
// "oauth" in it, and the code is on the line after the prompt for it.
func TestCodexDeviceSignInShowsTheLinkAndTheCode(t *testing.T) {
	out := "\x1b[0m\nWelcome to Codex [v\x1b[90m0.163.0\x1b[0m]\n\nFollow these steps to sign in with ChatGPT using device code authorization:\n\n1. Open this link in your browser and sign in to your account\n   \x1b[94mhttps://auth.openai.com/codex/device\x1b[0m\n\n2. Enter this one-time code \x1b[90m(expires in 15 minutes)\x1b[0m\n   \x1b[94mCS7W-9CFUS\x1b[0m\n"
	l := &Login{spec: codexLoginSpec("codex"), state: LoginStarting, buf: []byte(out)}
	l.parseLocked()
	if l.state != LoginAuthorize || l.url != "https://auth.openai.com/codex/device" || l.userCode != "CS7W-9CFUS" {
		t.Fatalf("%q %q %q", l.state, l.url, l.userCode)
	}
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte(`{"tokens":{"refresh_token":"r"}}`), 0o600)
	if l.spec.fromDisk(home) == "" {
		t.Fatal("the session on disk is not found")
	}
}
