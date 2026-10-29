package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/nodes"
	"github.com/dcimorra/the-middleware/internal/relocate"
)

// meshTo is f's server as a move's target over plain HTTP, through the
// same meshTarget the mesh uses, answering errors as the mesh does.
func meshTo(f *fixture) meshTarget {
	return meshTarget{x: nodes.Member{Name: "node-b"}, call: httpCall(f)}
}

func httpCall(f *fixture) func(ctx context.Context, method, path string, body, out any) error {
	return func(ctx context.Context, method, path string, body, out any) error {
		var rd io.Reader
		ctype := "application/json"
		switch b := body.(type) {
		case nil:
		case io.Reader:
			rd, ctype = b, "application/gzip"
		default:
			raw, _ := json.Marshal(b)
			rd = bytes.NewReader(raw)
		}
		req, _ := http.NewRequestWithContext(ctx, method, f.http.URL+"/api/v1"+path, rd)
		req.Header.Set("Authorization", "Bearer "+f.token)
		req.Header.Set("Content-Type", ctype)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		if res.StatusCode/100 != 2 {
			var e struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(raw, &e)
			return &nodes.RemoteError{Status: res.StatusCode, Message: e.Error}
		}
		if out != nil {
			return json.Unmarshal(raw, out)
		}
		return nil
	}
}

// A target that restarts forgets its jobs, and answers 404 for them from
// then on: the moving node stops asking at once, instead of asking for the
// move's hour with the site stopped. One that answers for another job than
// the one this move started is not waited on either.
func TestAMoveDoesNotWaitForAJobTheTargetForgot(t *testing.T) {
	b := newFixture(t).withToken()
	rc := relocate.Receiver{Root: b.root}
	os.MkdirAll(rc.StagingSite("p.test"), 0o755)

	restart := func(swap func(*prepareJobs)) meshTarget {
		tg := meshTo(b)
		inner := tg.call
		tg.call = func(ctx context.Context, method, path string, body, out any) error {
			err := inner(ctx, method, path, body, out)
			if method == "POST" && err == nil {
				swap(&b.srv.prepares)
			}
			return err
		}
		return tg
	}
	forgot := restart(func(p *prepareJobs) {
		p.mu.Lock()
		p.jobs = nil
		p.mu.Unlock()
	})
	if _, err := forgot.Prepare(context.Background(), "p.test", nil); err == nil || !strings.Contains(err.Error(), "no longer knows") {
		t.Fatalf("a forgotten install: %v", err)
	}
	other := restart(func(p *prepareJobs) {
		p.mu.Lock()
		p.jobs = map[string]*prepareJob{"p.test": {ID: "someone-elses", Done: true, Output: "not ours"}}
		p.mu.Unlock()
	})
	if _, err := other.Prepare(context.Background(), "p.test", nil); err == nil || !strings.Contains(err.Error(), "another install") {
		t.Fatalf("another job's answer: %v", err)
	}
	// And one that is the move's own is waited for, as before.
	if said, err := meshTo(b).Prepare(context.Background(), "p.test", nil); err != nil || !strings.Contains(said, "nothing to install") {
		t.Fatalf("its own install: %q %v", said, err)
	}
}
