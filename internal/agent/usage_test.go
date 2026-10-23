package agent

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAWindowWithoutAPercentageKeepsTheOneItHadAndTheBookSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	b := OpenUsageBook(path)
	b.Record("claude", "", "vendor", []Window{{Name: "7d", Used: 30}, {Name: "5h", Used: 10, ResetsAt: reset}})
	// A later turn says the 5h window is still allowed, without a number.
	b.Record("claude", "", "turn", []Window{{Name: "5h", Used: -1, ResetsAt: reset, Status: "allowed"}})
	u, _ := OpenUsageBook(path).Get("claude")
	if len(u.Windows) != 2 || u.Windows[0].Name != "5h" || u.Windows[0].Used != 10 || u.Windows[0].Status != "allowed" || u.Windows[1].Used != 30 {
		t.Fatalf("%+v", u)
	}
	// A new window (another reset) without a number is unknown, not the
	// old window's figure.
	b.Record("claude", "", "turn", []Window{{Name: "5h", Used: -1, ResetsAt: reset.Add(5 * time.Hour)}})
	if u, _ := b.Get("claude"); u.Windows[0].Used != -1 {
		t.Fatalf("%+v", u.Windows[0])
	}
}
