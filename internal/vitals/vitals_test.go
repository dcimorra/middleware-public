package vitals

import (
	"os"
	"testing"
	"time"
)

func TestAStretchUnderStrainIsAnEpisodeWithWhoCausedIt(t *testing.T) {
	m := New(Options{DataDir: t.TempDir()})
	now := time.Now()
	hot := func(i int) Sample { return Sample{At: now.Add(time.Duration(i) * every), CPU: 97} }
	for i := 0; i < openAfter-1; i++ {
		m.judge(hot(i))
	}
	if len(m.Episodes()) != 0 {
		t.Fatal("open before its time")
	}
	m.judge(hot(openAfter))
	eps := m.Episodes()
	if len(eps) != 1 || eps[0].Reason != "cpu" || !eps[0].To.IsZero() {
		t.Fatalf("not open: %+v", eps)
	}
	for i := 0; i < closeAfter; i++ {
		m.judge(Sample{At: now.Add(time.Duration(openAfter+1+i) * every), CPU: 10})
	}
	eps = m.Episodes()
	if len(eps) != 1 || eps[0].To.IsZero() || eps[0].PeakCPU != 97 {
		t.Fatalf("not closed: %+v", eps)
	}
	again := New(Options{DataDir: m.opts.DataDir})
	if len(again.Episodes()) != 1 {
		t.Fatal("the episode did not survive a restart")
	}
}

func TestTheMachineIsReadWhenThereIsAProc(t *testing.T) {
	if _, err := os.Stat("/proc/stat"); err != nil {
		t.Skip("no /proc")
	}
	m := New(Options{DataDir: t.TempDir()})
	m.sample(time.Now())
	time.Sleep(200 * time.Millisecond)
	m.sample(time.Now())
	s := m.Now()
	if s.Cores == 0 || s.MemTotal == 0 || s.DiskTotal == 0 {
		t.Fatalf("%+v", s)
	}
	if ps := m.procs(true); ps == nil && len(m.prevProc) == 0 {
		t.Fatal("no processes read")
	}
}

func TestBlameSumsWhoWasOnTop(t *testing.T) {
	b := blame([]Shot{
		{Procs: []Proc{{Cmd: "node build", Site: "a.example", CPU: 90}, {Cmd: "sshd", CPU: 1}}},
		{Procs: []Proc{{Cmd: "node build", Site: "a.example", CPU: 70}}},
	})
	if len(b) != 1 || b[0].Times != 2 || b[0].CPU != 80 || b[0].Site != "a.example" {
		t.Fatalf("%+v", b)
	}
}
