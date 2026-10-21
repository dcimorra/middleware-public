package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The panel's side of the site door: opening it for a site and choosing
// what it may do, and what every site spent through it. The settings go
// into the site's manifest, not the house's: the folder is the truth, and
// a year from now whoever reads site.json must see what this site was
// allowed without opening any screen. Opening the door restarts the site
// (the engine counts the token in its revision), so the switch means
// something in the process that serves and not only in the file.

// registerDoorPanel mounts /sites/{domain}/claude-api and /claude/sites.
func (s *Server) registerDoorPanel(o SiteDoorOptions) {
	s.Handle("GET", Prefix+"/sites/{domain}/claude-api", "The site's Claude door: open or not, its limits, its token, what it spent today and in the last 30 days", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		m, ok := o.Lookup(domain)
		if !ok || m == nil {
			return nil, Errorf(http.StatusNotFound, "%s: no such site", domain)
		}
		var models []string
		if o.Models != nil {
			models = o.Models()
		}
		out := map[string]any{
			"enabled":   m.ClaudeAPI != nil,
			"models":    models,
			"defaults":  rulesOf(nil, o.houseDaily()),
			"spent_usd": round4(o.spent(domain)),
			"api":       o.API,
			"socket":    o.Socket,
			"process":   m.Run != nil || len(m.Workers) > 0,
		}
		if _, uses, _ := o.usage(30); uses != nil {
			for _, u := range uses {
				if u.Domain == domain {
					out["use"] = u
				}
			}
		}
		if m.ClaudeAPI != nil {
			out["limits"] = rulesOf(m.ClaudeAPI, o.houseDaily())
			if o.Tokens != nil {
				// Shown whole on purpose: it is no secret of the house (it
				// speaks of this domain only, through this door only), and
				// whoever looks at this screen can stop the whole site.
				if tok, err := o.Tokens.Token(domain); err == nil {
					out["token"] = tok
				}
			}
			out["chats"] = len(o.siteChats(domain))
		}
		return out, nil
	})
	s.Handle("POST", Prefix+"/sites/{domain}/claude-api", "Open, close or tune the site's Claude door (body: enabled, tools, daily_usd, max_chats, model, hide_chats); it is written in the manifest and the site restarts", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		var b struct {
			Enabled  bool    `json:"enabled"`
			Tools    string  `json:"tools"`
			DailyUSD float64 `json:"daily_usd"`
			MaxChats int     `json:"max_chats"`
			Model    string  `json:"model"`
			Hide     bool    `json:"hide_chats"`
		}
		if err := ReadJSON(r, &b); err != nil {
			return nil, err
		}
		m, ok := o.Lookup(domain)
		if !ok || m == nil {
			return nil, Errorf(http.StatusNotFound, "%s: no such site", domain)
		}
		var value json.RawMessage = json.RawMessage("null")
		if b.Enabled {
			switch strings.TrimSpace(b.Tools) {
			case "", "none", "read", "web", "write":
			default:
				return nil, Errorf(http.StatusBadRequest, "tools is none, read, web or write")
			}
			if err := o.knownModel(b.Model); err != nil {
				return nil, err
			}
			if m.Run == nil && len(m.Workers) == 0 {
				return nil, Errorf(http.StatusBadRequest, "%s serves files and has no workers: no process of its own could call the door", domain)
			}
			// Only what differs from the defaults is written: a manifest
			// with four numbers that are exactly the defaults lies to whoever
			// comes next, it looks as if someone chose them.
			block := map[string]any{}
			if t := strings.TrimSpace(b.Tools); t != "" && t != "none" {
				block["tools"] = t
			}
			if b.DailyUSD > 0 && b.DailyUSD != o.houseDaily() {
				block["daily_usd"] = b.DailyUSD
			}
			if b.MaxChats > 0 && b.MaxChats != DefaultMaxChats {
				block["max_chats"] = b.MaxChats
			}
			if v := strings.TrimSpace(b.Model); v != "" {
				block["model"] = v
			}
			if b.Hide {
				block["hide_chats"] = true
			}
			value, _ = json.Marshal(block)
		}
		mgr, err := s.sitesManager()
		if err != nil {
			return nil, err
		}
		if _, err := mgr.Patch(domain, map[string]json.RawMessage{"claude_api": value}); err != nil {
			return nil, err
		}
		out := map[string]any{"ok": true}
		if b.Enabled {
			if o.Tokens != nil {
				if tok, err := o.Tokens.Token(domain); err == nil {
					out["token"] = tok
				}
			}
			// Open, and the first call would meet "no Claude Code here",
			// which leads nowhere: said now instead.
			if o.Available != nil && !o.Available() {
				out["warn"] = "open, but the Claude module has no session to use: sign in under Modules"
			}
		}
		return out, nil
	})
	s.Handle("GET", Prefix+"/claude/sites", "What every site spent through its Claude door: today, 7 and 30 days, this month, in total, per model, with a daily series (?days=7…90, 30 by default)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		n, _ := strconv.Atoi(r.URL.Query().Get("days"))
		if n < 7 || n > 90 {
			n = 30
		}
		days, uses, total := o.usage(n)
		type row struct {
			SiteUse
			Open     bool    `json:"open"`
			Exists   bool    `json:"exists"`
			DailyUSD float64 `json:"daily_usd"`
			Tools    string  `json:"tools,omitempty"`
		}
		seen := map[string]bool{}
		rows := []row{}
		for _, u := range uses {
			rw := row{SiteUse: u}
			if m, ok := o.Lookup(u.Domain); ok && m != nil {
				rw.Exists = true
				if m.ClaudeAPI != nil {
					rl := rulesOf(m.ClaudeAPI, o.houseDaily())
					rw.Open, rw.DailyUSD, rw.Tools = true, rl.DailyUSD, rl.Tools
				}
			}
			seen[u.Domain] = true
			rows = append(rows, rw)
		}
		// Open and at zero is a fact too: without them the list would look
		// incomplete.
		if o.Domains != nil {
			for _, d := range o.Domains() {
				if seen[d] {
					continue
				}
				if m, ok := o.Lookup(d); ok && m != nil && m.ClaudeAPI != nil {
					rl := rulesOf(m.ClaudeAPI, o.houseDaily())
					rows = append(rows, row{SiteUse: SiteUse{Domain: d, Series: make([]float64, n), Models: map[string]float64{}}, Open: true, Exists: true, DailyUSD: rl.DailyUSD, Tools: rl.Tools})
				}
			}
		}
		return map[string]any{"days": days, "sites": rows, "total": total}, nil
	})
}

// SiteUse is what a site spent through its door, summed.
type SiteUse struct {
	Domain  string             `json:"domain"`
	Today   float64            `json:"today"`
	Days7   float64            `json:"d7"`
	Days30  float64            `json:"d30"`
	Month   float64            `json:"month"`
	Total   float64            `json:"total"`
	Turns30 int                `json:"turns30"`
	Series  []float64          `json:"series"`
	Models  map[string]float64 `json:"models,omitempty"`
	Since   string             `json:"since,omitempty"`
}

// usage reads every site's door book: the days of the series (oldest
// first, UTC like the cap), one row per site that ever spent, and the
// sum.
func (o SiteDoorOptions) usage(n int) ([]string, []SiteUse, SiteUse) {
	total := SiteUse{Series: make([]float64, n), Models: map[string]float64{}}
	if o.Chat == nil || o.Chat.Ledger() == nil {
		return nil, nil, total
	}
	now := time.Now().UTC()
	days := make([]string, n)
	idx := map[string]int{}
	for i := range days {
		days[i] = now.AddDate(0, 0, i-n+1).Format("2006-01-02")
		idx[days[i]] = i
	}
	today := now.Format("2006-01-02")
	d7 := now.AddDate(0, 0, -6).Format("2006-01-02")
	d30 := now.AddDate(0, 0, -29).Format("2006-01-02")
	month := now.Format("2006-01")
	l := o.Chat.Ledger()
	var out []SiteUse
	for _, book := range l.Books() {
		domain, ok := strings.CutSuffix(book, "#door")
		if !ok {
			continue
		}
		f := l.File(book)
		u := SiteUse{Domain: domain, Series: make([]float64, n), Models: map[string]float64{}}
		for day, usd := range f.Days {
			u.Total += usd
			if u.Since == "" || day < u.Since {
				u.Since = day
			}
			if day == today {
				u.Today += usd
			}
			if day >= d7 {
				u.Days7 += usd
			}
			if day >= d30 {
				u.Days30 += usd
				u.Turns30 += f.Turns[day]
				for m, v := range f.Models[day] {
					u.Models[m] += v
				}
			}
			if strings.HasPrefix(day, month) {
				u.Month += usd
			}
			if i, ok := idx[day]; ok {
				u.Series[i] += usd
			}
		}
		total.Today += u.Today
		total.Days7 += u.Days7
		total.Days30 += u.Days30
		total.Month += u.Month
		total.Total += u.Total
		total.Turns30 += u.Turns30
		for i, v := range u.Series {
			total.Series[i] += v
		}
		for m, v := range u.Models {
			total.Models[m] += v
		}
		out = append(out, roundUse(u))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Days30 != out[j].Days30 {
			return out[i].Days30 > out[j].Days30
		}
		return out[i].Domain < out[j].Domain
	})
	return days, out, roundUse(total)
}

func roundUse(u SiteUse) SiteUse {
	u.Today, u.Days7, u.Days30, u.Month, u.Total = round4(u.Today), round4(u.Days7), round4(u.Days30), round4(u.Month), round4(u.Total)
	for i := range u.Series {
		u.Series[i] = round4(u.Series[i])
	}
	for m := range u.Models {
		u.Models[m] = round4(u.Models[m])
	}
	return u
}
