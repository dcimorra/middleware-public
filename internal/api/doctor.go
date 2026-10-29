package api

import (
	"net/http"

	"github.com/dcimorra/the-middleware/internal/installer"
)

// DoctorCheck is one line of `middleware doctor`, as the panel reads it.
type DoctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// RegisterDoctor mounts GET /system/doctor: the checks `middleware doctor`
// runs on the machine (binary, units, loopback socket, helper, root,
// reserved ports, docker, fail2ban, Node, wireguard, disk), for an
// operator who has the panel and no shell. run is installer.Doctor for
// this installation. They only read, so a GET.
func (s *Server) RegisterDoctor(run func() ([]installer.Check, bool)) {
	s.Handle("GET", Prefix+"/system/doctor", "What `middleware doctor` says of this machine: one check per line, and whether all passed", func(w http.ResponseWriter, r *http.Request) (any, error) {
		checks, ok := run()
		out := make([]DoctorCheck, 0, len(checks))
		for _, c := range checks {
			out = append(out, DoctorCheck{Name: c.Name, OK: c.OK, Detail: c.Detail})
		}
		return map[string]any{"ok": ok, "checks": out}, nil
	})
}
