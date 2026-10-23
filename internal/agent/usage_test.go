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

func TestTheBookSaysWhenAFigureMovesAndKeepsQuietWhenNothingDid(t *testing.T) {
	b := OpenUsageBook("")
	var got []Usage
	b.OnChange(func(provider string, u Usage) {
		if provider == "openai" {
			got = append(got, u)
		}
	})
	reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	b.Record("openai", "plus", "vendor", []Window{{Name: "5h", Used: 12, ResetsAt: reset}})
	b.Record("openai", "plus", "vendor", []Window{{Name: "5h", Used: 12.01, ResetsAt: reset.Add(3 * time.Minute)}})
	b.Record("openai", "plus", "vendor", []Window{{Name: "5h", Used: 14, ResetsAt: reset}})
	if len(got) != 2 || got[1].Windows[0].Used != 14 {
		t.Fatalf("changes = %+v", got)
	}
}
