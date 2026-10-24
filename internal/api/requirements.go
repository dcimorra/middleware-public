package api

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/auth"
	"github.com/dcimorra/middleware-public/internal/helper"
	"github.com/dcimorra/middleware-public/internal/installer"
)

// The house's requirements, installed from Sistema (the helper's
// requirements.install). The run is in a unit of its own and writes its output
// to data/requirements.log; this reads that file, whose last line says how it
// ended, so the panel can follow it across reloads and across a restart of
// this process.

// requirementsStale is how long a log without its last line counts as running:
// longer, and the run was cut (a reboot, a kill) and is said as such.
const requirementsStale = 30 * time.Minute

// RequirementsStatus is what GET /system/requirements answers.
type RequirementsStatus struct {
	// State: none, running, finished, restart (finished, and the house must
	// restart to see docker's group), failed, interrupted.
	State   string    `json:"state"`
	Log     string    `json:"log"`
	Error   string    `json:"error,omitempty"`
	Updated time.Time `json:"updated,omitzero"`
}

func (s *Server) requirementsStatus() RequirementsStatus {
	path := helper.InstallLog(s.opts.Root)
	info, err := os.Stat(path)
	if err != nil {
		return RequirementsStatus{State: "none"}
	}
	raw, _ := os.ReadFile(path)
	text := string(raw)
	if len(text) > 256<<10 {
		text = "[…]\n" + text[len(text)-256<<10:]
	}
	st := RequirementsStatus{Log: text, Updated: info.ModTime(), State: "running"}
	last := ""
	if lines := strings.Split(strings.TrimSpace(text), "\n"); len(lines) > 0 {
		last = strings.TrimSpace(lines[len(lines)-1])
	}
	switch {
	case strings.HasPrefix(last, installer.PanelRestart):
		st.State = "restart"
	case strings.HasPrefix(last, installer.PanelFinished):
		st.State = "finished"
	case strings.HasPrefix(last, installer.PanelFailed):
		st.State, st.Error = "failed", strings.TrimPrefix(last, installer.PanelFailed)
	case time.Since(info.ModTime()) > requirementsStale:
		st.State, st.Error = "interrupted", "it stopped writing without saying how it ended"
	}
	return st
}

// RegisterRequirements mounts /system/requirements; run asks the helper to start it.
func (s *Server) RegisterRequirements(run func(ctx context.Context, browser bool) error) {
	s.Handle("GET", Prefix+"/system/requirements", "The house's dependencies installed from the panel: state (none, running, finished, restart, failed, interrupted) and the installer's output", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return s.requirementsStatus(), nil
	})
	s.Handle("POST", Prefix+"/system/requirements", "Install the house's dependencies (body: browser?): packages, docker and its group, nvm with the LTS, fail2ban and, with browser, a headless Chromium, through the helper in a unit of its own; nothing else of the machine is touched", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if c := auth.FromContext(r.Context()); len(c.Scopes) > 0 {
			return nil, Errorf(http.StatusForbidden, "only the operator installs the house's dependencies")
		}
		var body struct {
			Browser bool `json:"browser"`
		}
		if r.ContentLength != 0 {
			if err := ReadJSON(r, &body); err != nil {
				return nil, err
			}
		}
		if st := s.requirementsStatus(); st.State == "running" {
			return nil, Errorf(http.StatusConflict, "the dependencies are already being installed")
		}
		// The last run's log goes first, so what is read from now on is
		// this run's and never says the last one's ending.
		_ = os.Remove(helper.InstallLog(s.opts.Root))
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := run(ctx, body.Browser); err != nil {
			return nil, Errorf(http.StatusBadGateway, "the helper: %v", err)
		}
		return RequirementsStatus{State: "running"}, nil
	})
}
