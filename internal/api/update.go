package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/dcimorra/middleware-public/internal/update"
)

// RegisterUpdate mounts the self-update.
func (s *Server) RegisterUpdate(u *update.Updater) {
	s.Handle("GET", Prefix+"/system/update", "This program's version, the newest release and whether it updates itself (?check=1 asks GitHub, at most hourly)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if r.URL.Query().Get("check") == "1" {
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			return u.Check(ctx, false), nil
		}
		return u.Status(), nil
	})
	s.Handle("POST", Prefix+"/system/update/check", "Ask GitHub for the newest release now", func(w http.ResponseWriter, r *http.Request) (any, error) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		return u.Check(ctx, true), nil
	})
	s.Handle("POST", Prefix+"/system/update/install", "Install the newest release now through the helper and restart (sites keep running)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		// The download is the helper's and takes its time, not the tab's.
		ctx, cancel := context.WithTimeout(context.Background(), 11*time.Minute)
		defer cancel()
		v, err := u.Install(ctx, "operator")
		if errors.Is(err, update.ErrNothingNewer) {
			return nil, Errorf(http.StatusConflict, "%v", err)
		}
		if err != nil {
			return nil, Errorf(http.StatusBadGateway, "%v", err)
		}
		return map[string]any{"ok": true, "version": v}, nil
	})
}
