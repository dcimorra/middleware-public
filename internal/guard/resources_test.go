package guard

import (
	"testing"
	"time"
)

func TestASiteBusyForMinutesIsAnEpisodeWithAStartAndAnEnd(t *testing.T) {
	fp := newFakeProc(t)
	h := newHouse(t)
	a := h.site("a.example", 100, 33001)
	spec := procSpec{pid: 100, ppid: 1, comm: "server", cmdline: []string{"./server"}, cwd: a.Dir, startSec: 10, listen: []int{33001}}
	fp.add(spec)
	now := time.Unix(fp.bootUnix+600, 0)
	kills := &killLog{}
	g, bus := newGuard(t, fp, h, kills, now)
	g.now = func() time.Time { return now }
	ticks := int64(0)
	minute := func(cpuPercent int64) {
		ticks += cpuPercent * 60 // 100 ticks a second is one core
		fp.setUsage(spec, ticks, 1000)
		now = now.Add(time.Minute)
		g.Sweep()
	}
	g.Sweep()
	for i := 0; i < 7; i++ {
		minute(100)
	}
	r := g.Resources("a.example")
	if len(r.Samples) != 7 || r.Samples[0].CPU != 100 || r.Samples[0].RSS == 0 {
		t.Fatalf("samples %+v", r.Samples)
	}
	if len(r.Episodes) != 1 || !r.Episodes[0].End.IsZero() || r.Episodes[0].Kind != "cpu" {
		t.Fatalf("an open episode: %+v", r.Episodes)
	}
	for i := 0; i < 6; i++ {
		minute(5)
	}
	r = g.Resources("a.example")
	if len(r.Episodes) != 1 || r.Episodes[0].End.IsZero() || r.Episodes[0].End.Sub(r.Episodes[0].Start) < 5*time.Minute {
		t.Fatalf("a closed episode: %+v", r.Episodes)
	}
	starts, ends := 0, 0
	for _, e := range bus.Recent() {
		switch e.Type {
		case "resources.episode":
			starts++
		case "resources.episode_end":
			ends++
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("events: %d starts, %d ends", starts, ends)
	}
	// It survives a restart of the guard.
	g2, _ := newGuard(t, fp, h, kills, now)
	if len(g2.Resources("a.example").Episodes) != 1 {
		t.Fatal("the history was not kept on disk")
	}
}
