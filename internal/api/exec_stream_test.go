package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stream posts a streamed exec and returns the output and its trailers.
func (f *fixture) stream(domain string, body map[string]any) (int, string, http.Header) {
	f.t.Helper()
	body["stream"] = true
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", f.http.URL+"/api/v1/sites/"+domain+"/exec", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out), resp.Trailer
}

// A streamed exec sends the output as it comes and the exit code once it
// is known, in a trailer: what `middleware exec` prints and exits with.
func TestAStreamedExecSendsItsOutputAndThenItsExitCode(t *testing.T) {
	f := newFixture(t).withToken()
	if resp, body := f.call("POST", "/api/v1/sites", map[string]any{"domain": "ex.test"}); resp.StatusCode != 201 {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	code, out, tr := f.stream("ex.test", map[string]any{"args": []string{"sh", "-c", "echo one; echo two >&2; exit 4"}})
	if code != 200 || !strings.Contains(out, "one") || !strings.Contains(out, "two") || tr.Get(ExitCodeTrailer) != "4" {
		t.Fatalf("stream: %d %q %v", code, out, tr)
	}
	if code, _, _ := f.stream("ex.test", map[string]any{"args": []string{"true"}, "service": "web"}); code != 404 {
		t.Fatalf("a service on a site without services: %d", code)
	}
}

// A site's turn and a service door get through the house into the site's
// container and nowhere else: a command that would run on the machine
// would run as the supervisor, outside the jail the turn's own run in.
func TestAServiceDoorExecsIntoTheContainerOnly(t *testing.T) {
	f := newFixture(t).withToken()
	for _, d := range []string{"ex.test", "box.test", "other.test"} {
		if resp, body := f.call("POST", "/api/v1/sites", map[string]any{"domain": d}); resp.StatusCode != 201 {
			t.Fatalf("create %s: %d %v", d, resp.StatusCode, body)
		}
	}
	os.WriteFile(filepath.Join(f.root, "sites", "box.test", "site.json"),
		[]byte(`{"version":2,"run":{"runtime":"docker","docker":{"image":"alpine:3.20"},"command":["sleep","infinity"]}}`), 0o644)
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\n[ \"$1\" = run ] && echo \"docker $*\"\nexit 0\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	resp, body := f.call("POST", "/api/v1/auth/tokens", map[string]any{"name": "door", "scopes": []string{"exec:ex.test", "exec:box.test"}})
	if resp.StatusCode != 200 {
		t.Fatalf("token: %d %v", resp.StatusCode, body)
	}
	f.token = body["token"].(string)

	code, out, tr := f.stream("box.test", map[string]any{"args": []string{"npm", "install"}})
	if code != 200 || !strings.Contains(out, "alpine:3.20 npm install") || !strings.Contains(out, "--cap-drop ALL") || tr.Get(ExitCodeTrailer) != "0" {
		t.Fatalf("into the container: %d %q %v", code, out, tr)
	}
	if code, out, _ := f.stream("ex.test", map[string]any{"args": []string{"id"}}); code != 403 {
		t.Fatalf("on the machine: %d %q", code, out)
	}
	if resp, _ := f.call("POST", "/api/v1/sites/box.test/exec", map[string]any{"command": "id"}); resp.StatusCode != 403 {
		t.Fatalf("the panel's exec with a service door: %d", resp.StatusCode)
	}
	if code, _, _ := f.stream("other.test", map[string]any{"args": []string{"id"}}); code != 403 {
		t.Fatalf("another site: %d", code)
	}
}
