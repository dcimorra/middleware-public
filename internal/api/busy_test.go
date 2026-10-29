package api

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/relocate"
)

// What the updater asks before it restarts the house: a move from here,
// a job of one coming in, and a copy arriving all say a move is running,
// and nothing once they end.
func TestAMoveRunningOnEitherSideIsSaid(t *testing.T) {
	f := newFixture(t).withToken()
	if f.srv.MovesRunning() {
		t.Fatal("running with nothing")
	}
	j, _ := f.srv.newMove("m.test", "b", nil)
	if !f.srv.MovesRunning() {
		t.Fatal("a move from here")
	}
	f.srv.finish(j, relocate.Report{}, nil)
	if f.srv.MovesRunning() {
		t.Fatal("a move that ended")
	}
	f.srv.migrates.mu.Lock()
	f.srv.migrates.jobs = map[string]*prepareJob{"m.test": {ID: "1"}}
	f.srv.migrates.mu.Unlock()
	if !f.srv.MovesRunning() {
		t.Fatal("a data import for a move coming in")
	}
	f.srv.migrates.mu.Lock()
	f.srv.migrates.jobs["m.test"].Done = true
	f.srv.migrates.mu.Unlock()

	// A copy arriving, as long as its request lasts: a bundle whose first
	// bytes went and whose rest has not.
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	done := make(chan struct{})
	go func() {
		req, _ := http.NewRequest("PUT", f.http.URL+"/api/v1/relocate/m.test", pr)
		req.Header.Set("Authorization", "Bearer "+f.token)
		req.Header.Set("Content-Type", "application/gzip")
		if res, err := http.DefaultClient.Do(req); err == nil {
			res.Body.Close()
		}
		close(done)
	}()
	gz := gzip.NewWriter(pw)
	go func() {
		gz.Write(bytes.Repeat([]byte{0}, 100))
		gz.Flush()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !f.srv.MovesRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !f.srv.MovesRunning() {
		t.Fatal("a copy arriving")
	}
	pw.CloseWithError(io.ErrUnexpectedEOF)
	<-done
	// The client may hear of its own failure before the handler returns.
	for f.srv.MovesRunning() && time.Now().Before(deadline.Add(5*time.Second)) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.srv.MovesRunning() {
		t.Fatal("a copy that ended")
	}
}
