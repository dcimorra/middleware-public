package agent

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
)

// Window is one usage limit of a subscription: how much of it is spent
// and when it starts again. Name is the house's: "5h", "7d", "7d_opus"…
type Window struct {
	Name string `json:"name"`
	// Label is the vendor's own name for a window the house has no word
	// for (Antigravity's "Gemini Models"); empty for 5h, 7d…
	Label string `json:"label,omitempty"`
	// Used is a percentage, 0 to 100; negative when the vendor said the
	// window exists but not how much of it is gone.
	Used     float64   `json:"used"`
	ResetsAt time.Time `json:"resets_at,omitempty"`
	// Status is the vendor's word for it when it gave one: "allowed",
	// "allowed_warning", "rejected".
	Status string `json:"status,omitempty"`
}

// Usage is what is known of one provider's subscription limits.
type Usage struct {
	Plan    string   `json:"plan,omitempty"`
	Windows []Window `json:"windows,omitempty"`
	// At is when it was learned, and Source how: "turn" (a turn's own
	// stream said it) or "vendor" (asked of the vendor).
	At     time.Time `json:"at"`
	Source string    `json:"source"`
}

// UsageBook keeps the last usage seen per provider, on disk, so the panel
// shows what the last turn said even after a restart.
type UsageBook struct {
	mu       sync.Mutex
	path     string
	m        map[string]Usage
	onChange func(provider string, u Usage)
}

// OnChange is called after a record that moved a figure or a reset, so a
// chat open on the panel follows a long turn without asking.
func (b *UsageBook) OnChange(f func(provider string, u Usage)) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.onChange = f
	b.mu.Unlock()
}

// OpenUsageBook reads the book at path; one that is not there is empty.
func OpenUsageBook(path string) *UsageBook {
	b := &UsageBook{path: path, m: map[string]Usage{}}
	_ = fsx.ReadJSON(path, &b.m)
	return b
}

// Record merges windows into the provider's entry: a turn often reports
// one window at a time, and the others it did not mention stay as they
// were.
func (b *UsageBook) Record(provider, plan, source string, ws []Window) {
	if b == nil || len(ws) == 0 {
		return
	}
	b.mu.Lock()
	u := b.m[provider]
	before := u.Windows
	byName := map[string]Window{}
	for _, w := range u.Windows {
		byName[w.Name] = w
	}
	for _, w := range ws {
		if old, ok := byName[w.Name]; ok && w.Used < 0 && (w.ResetsAt.IsZero() || w.ResetsAt.Equal(old.ResetsAt)) {
			w.Used = old.Used
		}
		byName[w.Name] = w
	}
	u.Windows = nil
	for _, w := range byName {
		u.Windows = append(u.Windows, w)
	}
	sort.Slice(u.Windows, func(i, j int) bool { return windowRank(u.Windows[i].Name) < windowRank(u.Windows[j].Name) })
	if plan != "" {
		u.Plan = plan
	}
	u.At, u.Source = time.Now().UTC(), source
	b.m[provider] = u
	if b.path != "" {
		_ = os.MkdirAll(filepath.Dir(b.path), 0o700)
		_ = fsx.WriteJSON(b.path, b.m, 0o600)
	}
	f := b.onChange
	changed := !sameWindows(before, u.Windows)
	u.Windows = append([]Window(nil), u.Windows...)
	b.mu.Unlock()
	if f != nil && changed {
		f(provider, u)
	}
}

// sameWindows says two readings show the same thing, to a tenth of a
// point: a vendor asked every minute mostly answers what it said before.
// A reset within a quarter of an hour of the last is the same reset:
// Antigravity dates an untouched window seven days from the moment it
// is asked, so its reset moves on every answer.
func sameWindows(a, b []Window) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		gap := a[i].ResetsAt.Sub(b[i].ResetsAt)
		if gap < 0 {
			gap = -gap
		}
		if a[i].Name != b[i].Name || a[i].Status != b[i].Status || gap > 15*time.Minute || int(a[i].Used*10) != int(b[i].Used*10) {
			return false
		}
	}
	return true
}

// Get is the provider's last usage.
func (b *UsageBook) Get(provider string) (Usage, bool) {
	if b == nil {
		return Usage{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.m[provider]
	u.Windows = append([]Window(nil), u.Windows...)
	return u, ok
}

// windowRank puts the short window first and the long ones after.
func windowRank(name string) string {
	switch name {
	case "5h":
		return "0"
	case "7d":
		return "1"
	}
	return "2" + name
}
