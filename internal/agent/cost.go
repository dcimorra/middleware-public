package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dcimorra/middleware-public/internal/fsx"
)

// Ledger is the per-site cost record at data/agents/cost/<domain>.json: a
// sum per day, so a month's total is a sum over keys and a cap is checked
// before a turn starts rather than discovered on the invoice.
type Ledger struct {
	dir string
	mu  sync.Mutex
}

// LedgerFile is the on-disk shape.
type LedgerFile struct {
	Domain string             `json:"domain"`
	Days   map[string]float64 `json:"days"` // "2006-01-02" → USD
	Total  float64            `json:"total"`
}

// NewLedger returns a ledger under dataDir.
func NewLedger(dataDir string) *Ledger {
	return &Ledger{dir: filepath.Join(dataDir, "agents", "cost")}
}

func (l *Ledger) path(domain string) string {
	return filepath.Join(l.dir, domain+".json")
}

func (l *Ledger) read(domain string) LedgerFile {
	var f LedgerFile
	if err := fsx.ReadJSON(l.path(domain), &f); err != nil || f.Days == nil {
		f = LedgerFile{Domain: domain, Days: map[string]float64{}}
	}
	return f
}

// Add records usd for domain on the day of at. Zero and negative amounts
// are ignored: a provider that reports no cost must not create a file that
// reads as "free".
func (l *Ledger) Add(domain string, usd float64, at time.Time) error {
	if usd <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.read(domain)
	day := at.UTC().Format("2006-01-02")
	f.Days[day] += usd
	f.Total += usd
	return fsx.WriteJSON(l.path(domain), f, 0o644)
}

// Month sums the domain's cost for the month containing at.
func (l *Ledger) Month(domain string, at time.Time) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.read(domain)
	prefix := at.UTC().Format("2006-01")
	sum := 0.0
	for day, v := range f.Days {
		if len(day) >= 7 && day[:7] == prefix {
			sum += v
		}
	}
	return sum
}

// Total returns everything ever spent by domain.
func (l *Ledger) Total(domain string) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.read(domain).Total
}

// ErrOverCap says the month's spending reached the manifest's cap.
type ErrOverCap struct {
	Domain string
	Spent  float64
	Cap    float64
}

func (e *ErrOverCap) Error() string {
	return fmt.Sprintf("%s: agent spending this month is $%.2f, cap is $%.2f", e.Domain, e.Spent, e.Cap)
}

// Allow fails with *ErrOverCap when cap is positive and the month is spent.
func (l *Ledger) Allow(domain string, cap float64, at time.Time) error {
	if cap <= 0 {
		return nil
	}
	if spent := l.Month(domain, at); spent >= cap {
		return &ErrOverCap{Domain: domain, Spent: spent, Cap: cap}
	}
	return nil
}

// MonthlyCap reads `agent.monthly_cap_usd` from a site folder's manifest
// without going through the manifest package: that package rejects unknown
// fields on purpose, and the field is read here so the schema stays owned
// by one place. A missing file or field is "no cap".
func MonthlyCap(siteDir string) float64 {
	data, err := os.ReadFile(filepath.Join(siteDir, "site.json"))
	if err != nil {
		return 0
	}
	var raw struct {
		Agent struct {
			MonthlyCapUSD float64 `json:"monthly_cap_usd"`
		} `json:"agent"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return 0
	}
	return raw.Agent.MonthlyCapUSD
}
