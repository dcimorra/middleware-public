package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// UsageEndpoint is where Claude Code itself asks for /usage: the account's
// five-hour and seven-day windows. A variable so a test can point it at a
// server of its own.
var UsageEndpoint = "https://api.anthropic.com/api/oauth/usage"

// Window is one cap of the account: how much is spent and when it resets.
type Window struct {
	Pct   float64    `json:"pct"`
	Reset *time.Time `json:"reset,omitempty"`
}

// Usage is the account's spend now. There is no command that prints it in
// -p mode (/usage exists only in the interactive interface), so it is asked
// where the interface asks, with the subscription's own token. The token is
// only read, never refreshed from here: rotating it from outside is how a
// machine loses its session to show a percentage.
type Usage struct {
	Session *Window   `json:"session,omitempty"` // five hours
	Week    *Window   `json:"week,omitempty"`    // seven days
	Opus    *Window   `json:"opus,omitempty"`    // when the plan caps it apart
	At      time.Time `json:"at"`
}

type windowJSON struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

func (w *windowJSON) window() *Window {
	if w == nil {
		return nil
	}
	out := &Window{Pct: w.Utilization}
	if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
		out.Reset = &t
	}
	return out
}

// FetchUsage asks the account's usage with token.
func FetchUsage(ctx context.Context, token string) (*Usage, error) {
	if token == "" {
		return nil, fmt.Errorf("no Claude subscription is signed in (agent-claude module)")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, UsageEndpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the Claude account answered %d", res.StatusCode)
	}
	var body struct {
		FiveHour     *windowJSON `json:"five_hour"`
		SevenDay     *windowJSON `json:"seven_day"`
		SevenDayOpus *windowJSON `json:"seven_day_opus"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}
	return &Usage{Session: body.FiveHour.window(), Week: body.SevenDay.window(), Opus: body.SevenDayOpus.window(), At: time.Now()}, nil
}
