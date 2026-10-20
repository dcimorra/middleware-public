package claude

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTheAccountsWindowsAreRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("anthropic-beta") == "" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"five_hour":{"utilization":42.5,"resets_at":"2026-10-20T18:00:00Z"},"seven_day":{"utilization":10},"extra":{}}`))
	}))
	defer srv.Close()
	defer func(u string) { UsageEndpoint = u }(UsageEndpoint)
	UsageEndpoint = srv.URL
	u, err := FetchUsage(context.Background(), "tok")
	if err != nil || u.Session.Pct != 42.5 || u.Session.Reset == nil || u.Week.Pct != 10 || u.Opus != nil {
		t.Fatalf("%+v %v", u, err)
	}
	if _, err := FetchUsage(context.Background(), "bad"); err == nil {
		t.Fatal("a 401 read as usage")
	}
}
