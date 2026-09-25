// Package cronx parses classic five-field cron expressions and answers two
// questions about them: does this expression fire at time t, and when does it
// fire next.
//
// It deliberately expands each field into a bitmask at parse time. That makes
// steps over ranges ("10-40/7") exact rather than the modulo approximation many
// small implementations use, and it makes Next cheap enough to call on render.
package cronx

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed cron expression.
type Schedule struct {
	expr string

	minute uint64 // bits 0-59
	hour   uint64 // bits 0-23
	dom    uint64 // bits 1-31
	month  uint64 // bits 1-12
	dow    uint64 // bits 0-6, Sunday = 0

	// domRestricted and dowRestricted implement Vixie cron's rule: when both
	// day fields are restricted the expression fires if *either* matches.
	domRestricted bool
	dowRestricted bool
}

// String returns the original expression.
func (s Schedule) String() string { return s.expr }

type fieldSpec struct {
	name     string
	min, max int
	names    map[string]int
}

var (
	minuteField = fieldSpec{"minute", 0, 59, nil}
	hourField   = fieldSpec{"hour", 0, 23, nil}
	domField    = fieldSpec{"day of month", 1, 31, nil}
	monthField  = fieldSpec{"month", 1, 12, map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}}
	dowField = fieldSpec{"day of week", 0, 6, map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}}
)

var shorthand = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

// Parse compiles a cron expression. Five space-separated fields are required,
// or one of the @shorthand aliases.
func Parse(expr string) (Schedule, error) {
	raw := strings.TrimSpace(expr)
	if raw == "" {
		return Schedule{}, fmt.Errorf("empty schedule")
	}
	lookup := strings.ToLower(raw)
	if alias, ok := shorthand[lookup]; ok {
		s, err := Parse(alias)
		s.expr = raw
		return s, err
	}
	fields := strings.Fields(raw)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("expected 5 fields (minute hour day month weekday), got %d", len(fields))
	}

	s := Schedule{expr: raw}
	var err error
	if s.minute, err = parseField(fields[0], minuteField); err != nil {
		return Schedule{}, err
	}
	if s.hour, err = parseField(fields[1], hourField); err != nil {
		return Schedule{}, err
	}
	if s.dom, err = parseField(fields[2], domField); err != nil {
		return Schedule{}, err
	}
	if s.month, err = parseField(fields[3], monthField); err != nil {
		return Schedule{}, err
	}
	if s.dow, err = parseField(fields[4], dowField); err != nil {
		return Schedule{}, err
	}
	s.domRestricted = !isWildcard(fields[2])
	s.dowRestricted = !isWildcard(fields[4])
	return s, nil
}

// Valid reports whether expr parses.
func Valid(expr string) error {
	_, err := Parse(expr)
	return err
}

func isWildcard(f string) bool { return f == "*" || f == "?" }

func parseField(field string, spec fieldSpec) (uint64, error) {
	var mask uint64
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return 0, fmt.Errorf("%s: empty list element in %q", spec.name, field)
		}
		m, err := parsePart(part, spec)
		if err != nil {
			return 0, err
		}
		mask |= m
	}
	if mask == 0 {
		return 0, fmt.Errorf("%s: %q never matches", spec.name, field)
	}
	return mask, nil
}

func parsePart(part string, spec fieldSpec) (uint64, error) {
	step := 1
	if slash := strings.Index(part, "/"); slash >= 0 {
		n, err := strconv.Atoi(part[slash+1:])
		if err != nil || n < 1 {
			return 0, fmt.Errorf("%s: step must be a positive number in %q", spec.name, part)
		}
		step = n
		part = part[:slash]
	}

	low, high := spec.min, spec.max
	switch {
	case part == "*" || part == "?":
		// full range
	case strings.Contains(part, "-"):
		bounds := strings.SplitN(part, "-", 2)
		var err error
		if low, err = parseValue(bounds[0], spec); err != nil {
			return 0, err
		}
		if high, err = parseValue(bounds[1], spec); err != nil {
			return 0, err
		}
		if low > high {
			return 0, fmt.Errorf("%s: range %q is inverted", spec.name, part)
		}
	default:
		v, err := parseValue(part, spec)
		if err != nil {
			return 0, err
		}
		low, high = v, v
		// "5/15" means "from 5 to the end of the range, every 15".
		if step > 1 {
			high = spec.max
		}
	}

	var mask uint64
	for v := low; v <= high; v += step {
		mask |= 1 << uint(v)
	}
	return mask, nil
}

func parseValue(s string, spec fieldSpec) (int, error) {
	s = strings.TrimSpace(s)
	if spec.names != nil {
		if v, ok := spec.names[strings.ToLower(s)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", spec.name, s)
	}
	// Cron traditionally accepts 7 for Sunday.
	if spec.name == "day of week" && v == 7 {
		v = 0
	}
	if v < spec.min || v > spec.max {
		return 0, fmt.Errorf("%s: %d is outside %d-%d", spec.name, v, spec.min, spec.max)
	}
	return v, nil
}

// Match reports whether the schedule fires during the minute containing t.
func (s Schedule) Match(t time.Time) bool {
	if s.minute&(1<<uint(t.Minute())) == 0 {
		return false
	}
	if s.hour&(1<<uint(t.Hour())) == 0 {
		return false
	}
	if s.month&(1<<uint(int(t.Month()))) == 0 {
		return false
	}
	domHit := s.dom&(1<<uint(t.Day())) != 0
	dowHit := s.dow&(1<<uint(int(t.Weekday()))) != 0
	switch {
	case s.domRestricted && s.dowRestricted:
		return domHit || dowHit
	case s.domRestricted:
		return domHit
	case s.dowRestricted:
		return dowHit
	default:
		return true
	}
}

// Next returns the first firing time strictly after t, or the zero time if the
// expression cannot fire within four years (for example 30 February).
func (s Schedule) Next(t time.Time) time.Time {
	// Start at the top of the next minute so Next is always strictly after t.
	c := t.Truncate(time.Minute).Add(time.Minute)
	limit := c.AddDate(4, 0, 0)
	for c.Before(limit) {
		if s.month&(1<<uint(int(c.Month()))) == 0 {
			c = time.Date(c.Year(), c.Month(), 1, 0, 0, 0, 0, c.Location()).AddDate(0, 1, 0)
			continue
		}
		if !s.dayMatches(c) {
			c = time.Date(c.Year(), c.Month(), c.Day(), 0, 0, 0, 0, c.Location()).AddDate(0, 0, 1)
			continue
		}
		if s.hour&(1<<uint(c.Hour())) == 0 {
			c = time.Date(c.Year(), c.Month(), c.Day(), c.Hour(), 0, 0, 0, c.Location()).Add(time.Hour)
			continue
		}
		if s.minute&(1<<uint(c.Minute())) == 0 {
			c = c.Add(time.Minute)
			continue
		}
		return c
	}
	return time.Time{}
}

func (s Schedule) dayMatches(t time.Time) bool {
	domHit := s.dom&(1<<uint(t.Day())) != 0
	dowHit := s.dow&(1<<uint(int(t.Weekday()))) != 0
	switch {
	case s.domRestricted && s.dowRestricted:
		return domHit || dowHit
	case s.domRestricted:
		return domHit
	case s.dowRestricted:
		return dowHit
	default:
		return true
	}
}

// Describe renders a short human sentence for an expression, used in the UI so
// operators do not have to read cron in their heads.
func Describe(expr string) string {
	s, err := Parse(expr)
	if err != nil {
		return "invalid schedule"
	}
	minutes := bitsIn(s.minute, 0, 59)
	hours := bitsIn(s.hour, 0, 23)

	everyMinute := len(minutes) == 60
	everyHour := len(hours) == 24

	var when string
	switch {
	case everyMinute && everyHour:
		when = "every minute"
	case len(minutes) == 1 && everyHour:
		when = fmt.Sprintf("every hour at %02d past", minutes[0])
	case len(minutes) == 1 && len(hours) == 1:
		when = fmt.Sprintf("at %02d:%02d", hours[0], minutes[0])
	case len(minutes) == 1 && evenlySpaced(hours):
		when = fmt.Sprintf("every %d hours at %02d past", hours[1]-hours[0], minutes[0])
	case evenlySpaced(minutes) && everyHour:
		when = fmt.Sprintf("every %d minutes", minutes[1]-minutes[0])
	case len(hours) == 1:
		when = fmt.Sprintf("%d times during the %02d:00 hour", len(minutes), hours[0])
	default:
		when = fmt.Sprintf("%d times a day", len(minutes)*len(hours))
	}

	var days string
	if s.dowRestricted {
		names := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
		var picked []string
		for _, d := range bitsIn(s.dow, 0, 6) {
			picked = append(picked, names[d])
		}
		days = " on " + strings.Join(picked, ", ")
	}
	if s.domRestricted {
		var picked []string
		for _, d := range bitsIn(s.dom, 1, 31) {
			picked = append(picked, ordinal(d))
		}
		if days != "" {
			days += " and the " + strings.Join(picked, ", ")
		} else {
			days = " on the " + strings.Join(picked, ", ")
		}
	}
	return when + days
}

func evenlySpaced(v []int) bool {
	if len(v) < 2 {
		return false
	}
	step := v[1] - v[0]
	for i := 2; i < len(v); i++ {
		if v[i]-v[i-1] != step {
			return false
		}
	}
	return true
}

func bitsIn(mask uint64, lo, hi int) []int {
	var out []int
	for i := lo; i <= hi; i++ {
		if mask&(1<<uint(i)) != 0 {
			out = append(out, i)
		}
	}
	return out
}

func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return strconv.Itoa(n) + suffix
}
