package agy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/agent"
)

// Usage asks the CLI for the subscription's limits: `agy -p /usage
// --output-format json` answers them as data, spends no tokens, and
// refreshes the sign-in itself, so the house never holds Google's client
// secret. home is a HOME of the module's own, with the sign-in seeded.
func Usage(ctx context.Context, bin, extraPath, home string) (string, []agent.Window, error) {
	cmd := exec.CommandContext(ctx, bin, "-p", "/usage", "--output-format", "json")
	cmd.Dir = home
	path := os.Getenv("PATH")
	if extraPath != "" {
		path = extraPath + string(os.PathListSeparator) + path
	}
	env := append(os.Environ(), "HOME="+home, "PATH="+path)
	for k, v := range Env() {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return "", nil, fmt.Errorf("%v: %s", err, msg)
	}
	return ParseUsage(out.Bytes())
}

// ParseUsage reads what /usage answers in json: groups of models sharing
// a limit, each with its buckets (a weekly one, so far).
func ParseUsage(raw []byte) (string, []agent.Window, error) {
	var doc struct {
		Status  string `json:"status"`
		Error   string `json:"error"`
		Command struct {
			Data struct {
				Tier   string `json:"tier"`
				Groups []struct {
					Name    string `json:"name"`
					Buckets []struct {
						ID                string   `json:"id"`
						Name              string   `json:"name"`
						Window            string   `json:"window"`
						RemainingFraction *float64 `json:"remaining_fraction"`
						ResetTime         string   `json:"reset_time"`
					} `json:"buckets"`
				} `json:"groups"`
			} `json:"data"`
		} `json:"command"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &doc); err != nil {
		return "", nil, fmt.Errorf("agy /usage: %w", err)
	}
	if doc.Error != "" {
		return "", nil, errors.New(doc.Error)
	}
	var ws []agent.Window
	for _, g := range doc.Command.Data.Groups {
		for _, b := range g.Buckets {
			w := agent.Window{Name: b.ID, Label: g.Name, Used: -1}
			if w.Name == "" {
				w.Name = g.Name
			}
			if len(g.Buckets) > 1 && b.Name != "" {
				w.Label = g.Name + " · " + b.Name
			}
			if b.RemainingFraction != nil {
				w.Used = (1 - *b.RemainingFraction) * 100
				if *b.RemainingFraction <= 0 {
					w.Status = "rejected"
				}
			}
			if t, err := time.Parse(time.RFC3339, b.ResetTime); err == nil {
				w.ResetsAt = t.UTC()
			}
			ws = append(ws, w)
		}
	}
	if len(ws) == 0 {
		return "", nil, errors.New("the answer names no usage window")
	}
	return doc.Command.Data.Tier, ws, nil
}
