package api

import (
	"net/http"

	"github.com/dcimorra/middleware-public/internal/vitals"
)

// RegisterVitals mounts the machine's live numbers and its episodes.
func (s *Server) RegisterVitals(m *vitals.Monitor) {
	s.Handle("GET", Prefix+"/system/vitals", "The machine now and over the last hour (CPU with disk wait apart, memory, swap, disk, load), and the stretches under strain with the processes and sites that caused them", func(w http.ResponseWriter, r *http.Request) (any, error) {
		out := map[string]any{"now": m.Now(), "episodes": m.Episodes(), "lines": map[string]float64{"cpu": vitals.CPULine, "memory": vitals.MemoryLine}}
		if r.URL.Query().Get("history") != "0" {
			out["history"] = m.History()
		}
		return out, nil
	})
}
