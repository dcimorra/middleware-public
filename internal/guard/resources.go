package guard

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/fsx"
)

// A site's resources over time: CPU (percent of one core) and resident
// memory, summed over every process attributed to it, one sample a minute
// for a day. An episode is a stretch of five minutes or more above a
// threshold; it is an event when it starts and when it ends, so "the
// machine was slow last night" has a site and a time attached.

// Sample is one minute of a site.
type Sample struct {
	At  time.Time `json:"at"`
	CPU float64   `json:"cpu"` // percent of one core, averaged over the minute
	RSS int64     `json:"rss"` // bytes, at the sample
}

// Episode is a stretch above a threshold.
type Episode struct {
	Kind  string    `json:"kind"` // "cpu" or "memory"
	Start time.Time `json:"start"`
	End   time.Time `json:"end,omitzero"`
	Peak  float64   `json:"peak"` // percent for cpu, bytes for memory
}

// Resources is what is kept per site.
type Resources struct {
	Samples  []Sample  `json:"samples"`
	Episodes []Episode `json:"episodes"`
}

const (
	sampleEvery    = time.Minute
	keepSamples    = 24 * 60
	keepEpisodes   = 50
	episodeAfter   = 5 // minutes above the line before it is an episode
	cpuLine        = 90.0
	memoryLine     = int64(2 << 30)
	persistEvery   = 5 * time.Minute
	ticksPerSecond = 100
)

type siteMeter struct {
	res       Resources
	lastTicks int64
	lastAt    time.Time
	above     map[string]int // consecutive minutes above, by kind
	below     map[string]int
	open      map[string]*Episode
	persisted time.Time
}

type meters struct {
	mu    sync.Mutex
	sites map[string]*siteMeter
}

func (g *Guard) resourcesPath(domain string) string {
	return filepath.Join(g.opts.DataDir, "resources", domain+".json")
}

func (g *Guard) meter(domain string) *siteMeter {
	g.res.mu.Lock()
	defer g.res.mu.Unlock()
	if g.res.sites == nil {
		g.res.sites = map[string]*siteMeter{}
	}
	m := g.res.sites[domain]
	if m == nil {
		m = &siteMeter{above: map[string]int{}, below: map[string]int{}, open: map[string]*Episode{}}
		_ = fsx.ReadJSON(g.resourcesPath(domain), &m.res)
		g.res.sites[domain] = m
	}
	return m
}

// measure adds a site's processes to its meter; a sample is taken once a
// minute from the CPU ticks consumed since the last one.
func (g *Guard) measure(domain string, tree []*Process) {
	var ticks, rss int64
	for _, p := range tree {
		ticks += p.CPUTicks
		rss += p.RSS
	}
	now := g.now()
	m := g.meter(domain)
	g.res.mu.Lock()
	defer g.res.mu.Unlock()
	if m.lastAt.IsZero() {
		m.lastAt, m.lastTicks = now, ticks
		return
	}
	elapsed := now.Sub(m.lastAt)
	if elapsed < sampleEvery {
		return
	}
	delta := ticks - m.lastTicks
	if delta < 0 {
		delta = 0 // processes ended: their ticks left the sum
	}
	cpu := float64(delta) / ticksPerSecond / elapsed.Seconds() * 100
	m.lastAt, m.lastTicks = now, ticks
	s := Sample{At: now.UTC(), CPU: float64(int(cpu*10)) / 10, RSS: rss}
	m.res.Samples = append(m.res.Samples, s)
	if len(m.res.Samples) > keepSamples {
		m.res.Samples = m.res.Samples[len(m.res.Samples)-keepSamples:]
	}
	g.episode(domain, m, "cpu", s.CPU >= cpuLine, s.CPU, now)
	g.episode(domain, m, "memory", s.RSS >= memoryLine, float64(s.RSS), now)
	if now.Sub(m.persisted) >= persistEvery {
		m.persisted = now
		_ = fsx.WriteJSON(g.resourcesPath(domain), m.res, 0o644)
	}
}

// episode moves one kind's state by one sample. Called under the lock.
func (g *Guard) episode(domain string, m *siteMeter, kind string, high bool, value float64, now time.Time) {
	if high {
		m.above[kind]++
		m.below[kind] = 0
	} else {
		m.below[kind]++
		m.above[kind] = 0
	}
	ep := m.open[kind]
	switch {
	case ep == nil && m.above[kind] >= episodeAfter:
		ep = &Episode{Kind: kind, Start: now.Add(-episodeAfter * sampleEvery).UTC(), Peak: value}
		m.open[kind] = ep
		g.emit(events.Event{Type: "resources.episode", Subject: domain, Actor: "guard", Level: "warn",
			Message: fmt.Sprintf("%s above the line for %d minutes (%s)", kind, episodeAfter, show(kind, value)), Data: map[string]any{"kind": kind}})
	case ep != nil && value > ep.Peak:
		ep.Peak = value
	}
	if ep != nil && m.below[kind] >= episodeAfter {
		ep.End = now.Add(-episodeAfter * sampleEvery).UTC()
		m.res.Episodes = append(m.res.Episodes, *ep)
		if len(m.res.Episodes) > keepEpisodes {
			m.res.Episodes = m.res.Episodes[len(m.res.Episodes)-keepEpisodes:]
		}
		delete(m.open, kind)
		g.emit(events.Event{Type: "resources.episode_end", Subject: domain, Actor: "guard",
			Message: fmt.Sprintf("%s back below the line after %s, peak %s", kind, ep.End.Sub(ep.Start).Round(time.Minute), show(kind, ep.Peak)), Data: map[string]any{"kind": kind}})
		_ = fsx.WriteJSON(g.resourcesPath(domain), m.res, 0o644)
	}
}

func show(kind string, v float64) string {
	if kind == "cpu" {
		return fmt.Sprintf("%.0f%% of a core", v)
	}
	return fmt.Sprintf("%.1f GB", v/(1<<30))
}

// Resources is a site's history, with any episode still open at the end.
func (g *Guard) Resources(domain string) Resources {
	m := g.meter(domain)
	g.res.mu.Lock()
	defer g.res.mu.Unlock()
	out := Resources{Samples: append([]Sample{}, m.res.Samples...), Episodes: append([]Episode{}, m.res.Episodes...)}
	for _, ep := range m.open {
		out.Episodes = append(out.Episodes, *ep)
	}
	return out
}
