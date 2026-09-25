package cronx

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func TestStepOverRangeIsExactNotModulo(t *testing.T) {
	// The naive "v %% n == 0" implementation gets this wrong: it would match
	// 14, 21, 28 and miss 10, 17, 24.
	s := mustParse(t, "10-30/7 * * * *")
	want := map[int]bool{10: true, 17: true, 24: true}
	for m := 0; m < 60; m++ {
		tm := time.Date(2026, 3, 2, 12, m, 0, 0, time.UTC)
		if got := s.Match(tm); got != want[m] {
			t.Fatalf("minute %d: match=%v want=%v", m, got, want[m])
		}
	}
}

func TestWildcardStep(t *testing.T) {
	s := mustParse(t, "0 */5 * * *")
	for h := 0; h < 24; h++ {
		tm := time.Date(2026, 3, 2, h, 0, 0, 0, time.UTC)
		if s.Match(tm) != (h%5 == 0) {
			t.Fatalf("hour %d mismatched", h)
		}
	}
}

func TestNamesAndSundaySeven(t *testing.T) {
	s := mustParse(t, "0 4 * jan-mar sun")
	seven := mustParse(t, "0 4 * 1-3 7")
	sunday := time.Date(2026, 2, 1, 4, 0, 0, 0, time.UTC) // a Sunday
	if !s.Match(sunday) || !seven.Match(sunday) {
		t.Fatal("named months and weekday 7 must both match Sunday")
	}
	if s.Match(time.Date(2026, 4, 5, 4, 0, 0, 0, time.UTC)) {
		t.Fatal("April is outside jan-mar")
	}
}

// Vixie cron fires when *either* day field matches if both are restricted.
func TestDayOfMonthOrDayOfWeek(t *testing.T) {
	s := mustParse(t, "0 0 13 * fri")
	friday := time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC) // Friday, not the 13th
	thirteenth := time.Date(2026, 4, 13, 0, 0, 0, 0, time.UTC)
	if !s.Match(friday) || !s.Match(thirteenth) {
		t.Fatal("restricted dom and dow must be OR-ed")
	}

	onlyDom := mustParse(t, "0 0 13 * *")
	if onlyDom.Match(friday) {
		t.Fatal("unrestricted dow must not widen a restricted dom")
	}
}

func TestNextSkipsForward(t *testing.T) {
	s := mustParse(t, "30 4 * * *")
	from := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	next := s.Next(from)
	want := time.Date(2026, 3, 3, 4, 30, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("Next = %s, want %s", next, want)
	}
	// Next must be strictly after the input even when the input matches.
	onTime := time.Date(2026, 3, 3, 4, 30, 0, 0, time.UTC)
	if !s.Next(onTime).Equal(want.AddDate(0, 0, 1)) {
		t.Fatalf("Next from a matching minute must advance, got %s", s.Next(onTime))
	}
}

func TestNextGivesUpOnImpossibleDates(t *testing.T) {
	s := mustParse(t, "0 0 30 2 *") // 30 February
	if !s.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).IsZero() {
		t.Fatal("an impossible date must return the zero time rather than loop")
	}
}

// Schedules run in the operator's timezone, so a 04:00 restart must stay at
// 04:00 local across a daylight saving transition.
func TestNextRespectsLocalTimezone(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skip("tzdata unavailable in this environment")
	}
	s := mustParse(t, "0 4 * * *")
	// UK clocks go forward on 29 March 2026.
	from := time.Date(2026, 3, 28, 12, 0, 0, 0, london)
	next := s.Next(from)
	if next.Hour() != 4 {
		t.Fatalf("expected 04:00 local, got %s", next)
	}
	if next.Day() != 29 {
		t.Fatalf("expected the transition day, got %s", next)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	bad := []string{"", "* * * *", "* * * * * *", "60 * * * *", "* 25 * * *",
		"*/0 * * * *", "5-1 * * * *", "abc * * * *", "0 0 * * xyz"}
	for _, expr := range bad {
		if err := Valid(expr); err == nil {
			t.Fatalf("expected %q to be rejected", expr)
		}
	}
}

func TestShorthand(t *testing.T) {
	s := mustParse(t, "@daily")
	if !s.Match(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("@daily should fire at midnight")
	}
	if s.String() != "@daily" {
		t.Fatal("String should preserve the original expression")
	}
}

func TestDescribe(t *testing.T) {
	cases := map[string]string{
		"0 4 * * *":     "at 04:00",
		"0 */2 * * *":   "every 2 hours at 00 past",
		"*/15 * * * *":  "every 15 minutes",
		"0 0 * * 0":     "at 00:00 on Sun",
		"30 3 1 * *":    "at 03:30 on the 1st",
		"nonsense here": "invalid schedule",
	}
	for expr, want := range cases {
		if got := Describe(expr); got != want {
			t.Fatalf("Describe(%q) = %q, want %q", expr, got, want)
		}
	}
}
