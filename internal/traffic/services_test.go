package traffic

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheAccessLogSaysWhichServiceAnswered(t *testing.T) {
	dir := t.TempDir()
	c := newClock()
	rec := New(Options{LogDir: dir, Now: c.now, Service: func(r *http.Request) string {
		if strings.HasPrefix(r.URL.Path, "/api") {
			return "api"
		}
		return ""
	}})
	h := rec.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	for _, p := range []string{"/api/x", "/"} {
		r := httptest.NewRequest("GET", "http://log.example"+p, nil)
		r.RemoteAddr = "203.0.113.9:1234"
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	rec.Close()
	data, err := os.ReadFile(filepath.Join(dir, "log.example", "access.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], " service=api") || strings.Contains(lines[1], "service=") {
		t.Fatalf("lines %q", lines)
	}
}
