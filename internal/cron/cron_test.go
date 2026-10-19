package cron

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNextMatchesTheTable(t *testing.T) {
	table := []struct{ expr, after, want string }{
		{"* * * * *", "2026-10-19 10:00", "2026-10-19 10:01"},
		{"* * * * *", "2026-10-19 10:00", "2026-10-19 10:01"},
		{"0 3 * * *", "2026-10-19 10:00", "2026-10-20 03:00"},
		{"0 3 * * *", "2026-10-19 02:59", "2026-10-19 03:00"},
		{"0 3 * * *", "2026-10-19 03:00", "2026-10-20 03:00"},
		{"*/15 * * * *", "2026-10-19 10:07", "2026-10-19 10:15"},
		{"*/15 * * * *", "2026-10-19 10:45", "2026-10-19 11:00"},
		{"5,35 9-17 * * 1-5", "2026-10-16 17:36", "2026-10-19 09:05"}, // Fri → Mon
		{"5,35 9-17 * * mon-fri", "2026-10-16 17:36", "2026-10-19 09:05"},
		{"0 0 1 * *", "2026-10-19 10:00", "2026-11-01 00:00"},
		{"0 0 1 jan *", "2026-10-19 10:00", "2027-01-01 00:00"},
		{"0 0 29 2 *", "2026-10-19 10:00", "2028-02-29 00:00"},
		{"0 0 1,15 * 1", "2026-10-19 10:00", "2026-10-26 00:00"}, // Mon 26th before Nov 1st (dom OR dow)
		{"0 0 * * 0", "2026-10-19 10:00", "2026-10-25 00:00"},
		{"0 0 * * 7", "2026-10-19 10:00", "2026-10-25 00:00"},
		{"30 4 10-20/5 * *", "2026-10-01 00:00", "2026-10-10 04:30"},
		{"30 4 10-20/5 * *", "2026-10-10 04:30", "2026-10-15 04:30"},
		{"0 12 5/10 * *", "2026-10-06 00:00", "2026-10-15 12:00"},
		{"@hourly", "2026-10-19 10:07", "2026-10-19 11:00"},
		{"@daily", "2026-10-19 10:07", "2026-10-20 00:00"},
		{"@weekly", "2026-10-19 10:07", "2026-10-25 00:00"},
		{"@monthly", "2026-10-19 10:07", "2026-11-01 00:00"},
		{"59 23 31 12 *", "2026-10-19 10:07", "2026-12-31 23:59"},
	}
	for _, row := range table {
		s, err := Parse(row.expr)
		if err != nil {
			t.Errorf("%q: %v", row.expr, err)
			continue
		}
		got := s.Next(at(row.after))
		if got.Format("2006-01-02 15:04") != row.want {
			t.Errorf("%q after %s: got %s, want %s", row.expr, row.after, got.Format("2006-01-02 15:04"), row.want)
		}
		if !s.Matches(got) {
			t.Errorf("%q: Next returned %s but Matches says no", row.expr, got)
		}
	}
}

func TestNextDropsSecondsAndIsStrictlyAfter(t *testing.T) {
	s, _ := Parse("* * * * *")
	after := time.Date(2026, 10, 19, 10, 0, 30, 0, time.UTC)
	if got := s.Next(after); !got.Equal(time.Date(2026, 10, 19, 10, 1, 0, 0, time.UTC)) {
		t.Fatalf("got %s", got)
	}
}

func TestAnImpossibleScheduleReturnsTheZeroTime(t *testing.T) {
	s, err := Parse("0 0 31 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Next(at("2026-10-19 10:00")); !got.IsZero() {
		t.Fatalf("got %s", got)
	}
}

func TestBadExpressionsAreRejected(t *testing.T) {
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8", "*/0 * * * *", "5-3 * * * *", "a * * * *", "@never", "1,,2 * * * *"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
