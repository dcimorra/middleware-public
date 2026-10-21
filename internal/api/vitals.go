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
	s.AddSummary("vitals", func() any {
		return map[string]any{"now": m.Now(), "minutes": perMinute(m.History()), "open_episode": openEpisode(m.Episodes())}
	})
}

// perMinute folds the hour of samples into one point a minute, which is
// what a card a few hundred pixels wide can draw and what crosses the wire
// for every node every few seconds.
func perMinute(h []vitals.Sample) [][3]float64 {
	out := [][3]float64{}
	var cpu, mem float64
	var n int
	var cur int64 = -1
	flush := func() {
		if n > 0 {
			out = append(out, [3]float64{float64(cur * 60), round1(cpu / float64(n)), round1(mem / float64(n))})
		}
	}
	for _, s := range h {
		m := s.At.Unix() / 60
		if m != cur {
			flush()
			cur, cpu, mem, n = m, 0, 0, 0
		}
		cpu += s.CPU
		mem += s.MemPct
		n++
	}
	flush()
	return out
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

func openEpisode(eps []vitals.Episode) any {
	for _, e := range eps {
		if e.To.IsZero() {
			// Without the shots: the overview wants to know that the
			// machine is struggling and who is to blame, not the detail.
			blamed := e.Blamed
			if len(blamed) > 3 {
				blamed = blamed[:3]
			}
			return map[string]any{"from": e.From, "reason": e.Reason, "peak_cpu": e.PeakCPU, "peak_mem": e.PeakMem, "blamed": blamed}
		}
	}
	return nil
}
