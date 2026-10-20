package shots

import (
	"context"
	"io"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

// A real browser takes a real picture of what the bare handler serves.
// It runs where a headless Chromium exists (SHOT=1 insists on it).
func TestAPictureIsTakenThroughTheSitesOwnDoor(t *testing.T) {
	sh := New(Options{DataDir: t.TempDir(), Bare: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<body style='background:#c00'><h1>"+r.Host+"</h1></body>")
	})})
	if !sh.Available() {
		if os.Getenv("SHOT") == "1" {
			t.Fatal("SHOT=1 and no headless Chromium found")
		}
		t.Skip("no headless Chromium")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.Run(ctx)
	sh.Request("pic.test", true)
	deadline := time.Now().Add(60 * time.Second)
	for {
		if p, _, ok := sh.Path("pic.test"); ok {
			data, _ := os.ReadFile(p)
			if len(data) < 1000 || string(data[1:4]) != "PNG" {
				t.Fatalf("not a picture: %d bytes", len(data))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no picture after a minute")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestTheDoorAnswersOnlyItsOwnSite(t *testing.T) {
	sh := New(Options{Binary: "none", Bare: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })})
	port, shut, err := sh.door("a.test")
	if err != nil {
		t.Fatal(err)
	}
	defer shut()
	get := func(host string) int {
		req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(port)+"/", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if get("a.test") != 200 || get("b.test") != 404 {
		t.Fatal("the door answered another site")
	}
}
