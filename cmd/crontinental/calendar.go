package main

// A native implementation of systemd's calendar event grammar (systemd.time(7),
// "Calendar Events"), so `every` means the same thing on every platform. On
// Linux the tests check it against `systemd-analyze calendar` across a corpus
// of specs, base times and time zones; where this file and systemd disagree,
// this file is wrong.
//
// One deliberate omission: fractional seconds ("12:30:15.5") are refused. The
// tick runs once a minute, so a fraction can never matter.

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	minYear = 1970
	maxYear = 2199 // systemd's own ceiling
)

// item is one comma-separated piece of a component: a value, a range, either
// optionally repeating.
type item struct {
	start, end, rep int  // end == start for a single value; rep 0 = none
	isRange         bool // "N..M", even when N == M: it bounds a repeat where a lone N doesn't
}

// field is one component of the spec (year, month, day, hour, ...). Empty
// items mean "*".
type field struct {
	items []item
}

func (f field) any() bool { return len(f.items) == 0 }

func (f field) match(v int) bool {
	if f.any() {
		return true
	}
	for _, it := range f.items {
		if it.matches(v) {
			return true
		}
	}
	return false
}

// matches: a repeat walks forward from the start ("the value itself and the
// value plus all multiples of the repetition value"), stopping at the range's
// end when there is one.
func (it item) matches(v int) bool {
	if it.rep == 0 {
		return v >= it.start && v <= it.end
	}
	if v < it.start || (it.isRange && v > it.end) {
		return false
	}
	return (v-it.start)%it.rep == 0
}

// lastValue is a range's last real value: systemd ends "00..30/4" at 28. A
// single value (repeating or not) is its own.
func (it item) lastValue() int {
	if it.isRange && it.rep != 0 {
		return it.start + (it.end-it.start)/it.rep*it.rep
	}
	return it.end
}

// matchFromEnd is the day field of a "~" spec, counted back from the end of
// the month: ~01 is the last day. A repeat walks toward the end of the month,
// so "~07/1" is the last seven days.
//
// A range, though, steps forward through itself like any other: "~07..16/5"
// is the 7th- and 12th-last days.
func (it item) matchFromEnd(back int) bool {
	if it.rep == 0 {
		return back >= it.start && back <= it.end
	}
	if it.isRange {
		return back >= it.start && back <= it.end && (back-it.start)%it.rep == 0
	}
	return back <= it.start && (it.start-back)%it.rep == 0
}

type calendar struct {
	weekdays          uint8 // bit 0 = Monday; 0 = any day
	year, month, day  field
	endOfMonth        bool // the day field counts back from the end ("~")
	hour, minute, sec field
	loc               *time.Location // nil = the machine's local zone
	tzName            string
	epoch             *time.Time // "@1700000000": one absolute moment
}

var shorthands = map[string]string{
	"minutely":      "*-*-* *:*:00",
	"hourly":        "*-*-* *:00:00",
	"daily":         "*-*-* 00:00:00",
	"monthly":       "*-*-01 00:00:00",
	"weekly":        "Mon *-*-* 00:00:00",
	"yearly":        "*-01-01 00:00:00",
	"annually":      "*-01-01 00:00:00",
	"quarterly":     "*-01,04,07,10-01 00:00:00",
	"semiannually":  "*-01,07-01 00:00:00",
	"semi-annually": "*-01,07-01 00:00:00",
}

var weekdayNames = []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}

var errNotCalendar = errors.New("not a calendar spec")

func calErr(spec, why string) error {
	return fmt.Errorf("cannot read calendar spec %q: %s", spec, why)
}

// parseCalendar reads a calendar spec. An error means it isn't one, which is
// how `every 30m` falls through to the interval reading.
func parseCalendar(spec string) (*calendar, error) {
	if spec == "" || strings.TrimSpace(spec) != spec {
		return nil, calErr(spec, "empty, or space at either end")
	}
	tokens := strings.Fields(spec)
	c := &calendar{}

	if strings.HasPrefix(tokens[0], "@") && len(tokens) == 1 {
		secs, err := strconv.ParseInt(tokens[0][1:], 10, 64)
		if err != nil || secs < 0 {
			return nil, calErr(spec, "@ takes seconds since the epoch")
		}
		t := time.Unix(secs, 0).UTC()
		c.epoch, c.loc, c.tzName = &t, time.UTC, "UTC"
		return c, nil
	}

	// A trailing time zone: "UTC" (any case) or an IANA name.
	if len(tokens) > 1 {
		last := tokens[len(tokens)-1]
		if strings.EqualFold(last, "UTC") {
			c.loc, c.tzName = time.UTC, "UTC"
			tokens = tokens[:len(tokens)-1]
		} else if !strings.ContainsAny(last, ":*~") && !startsWithDigit(last) {
			if loc, err := time.LoadLocation(last); err == nil {
				c.loc, c.tzName = loc, last
				tokens = tokens[:len(tokens)-1]
			}
		}
	}

	if len(tokens) == 1 {
		if exp, ok := shorthands[strings.ToLower(tokens[0])]; ok {
			tokens = strings.Fields(exp)
		}
	}

	i := 0
	if i < len(tokens) && startsWithLetter(tokens[i]) {
		mask, err := parseWeekdays(tokens[i])
		if err != nil {
			return nil, calErr(spec, err.Error())
		}
		c.weekdays = mask
		i++
	}
	sawDate, sawTime := false, false
	if i < len(tokens) && !strings.Contains(tokens[i], ":") {
		if err := c.parseDate(tokens[i]); err != nil {
			return nil, calErr(spec, err.Error())
		}
		sawDate = true
		i++
	}
	if i < len(tokens) {
		if err := c.parseTime(tokens[i]); err != nil {
			return nil, calErr(spec, err.Error())
		}
		sawTime = true
		i++
	}
	if i != len(tokens) {
		return nil, calErr(spec, fmt.Sprintf("unexpected %q", tokens[i]))
	}
	if !sawDate && !sawTime && c.weekdays == 0 && len(tokens) == 0 {
		return nil, calErr(spec, "nothing to schedule")
	}
	if !sawTime {
		c.hour, c.minute, c.sec = exact(0), exact(0), exact(0)
	}
	// "Mon..Sun" restricts nothing; systemd normalizes it away.
	if c.weekdays == 0x7f {
		c.weekdays = 0
	}
	return c, nil
}

func exact(v int) field { return field{[]item{{v, v, 0, false}}} }

func startsWithLetter(s string) bool {
	return s != "" && (s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z')
}

func startsWithDigit(s string) bool { return s != "" && s[0] >= '0' && s[0] <= '9' }

func weekdayIndex(s string) (int, bool) {
	l := strings.ToLower(s)
	for i, n := range weekdayNames {
		if l == n || l == n[:3] {
			return i, true
		}
	}
	return 0, false
}

func parseWeekdays(tok string) (uint8, error) {
	var mask uint8
	for _, part := range strings.Split(tok, ",") {
		a, b, isRange := strings.Cut(part, "..")
		if !isRange {
			a, b, isRange = strings.Cut(part, "-")
		}
		from, ok := weekdayIndex(a)
		if !ok {
			return 0, fmt.Errorf("%q is not a weekday", a)
		}
		to := from
		if isRange {
			if to, ok = weekdayIndex(b); !ok {
				return 0, fmt.Errorf("%q is not a weekday", b)
			}
			if to < from {
				return 0, fmt.Errorf("weekday range %q runs backwards (ranges go Mon..Sun)", part)
			}
		}
		for d := from; d <= to; d++ {
			mask |= 1 << d
		}
	}
	return mask, nil
}

func (c *calendar) parseDate(tok string) error {
	var parts []string
	dayPart := ""
	if l, r, ok := strings.Cut(tok, "~"); ok {
		c.endOfMonth = true
		parts = strings.Split(l, "-")
		dayPart = r
		parts = append(parts, dayPart)
	} else {
		parts = strings.Split(tok, "-")
	}
	switch len(parts) {
	case 3:
	case 2:
		parts = append([]string{"*"}, parts...)
	default:
		return fmt.Errorf("a date is [YEAR-]MONTH-DAY, not %q", tok)
	}
	var err error
	if c.year, err = parseField(parts[0], minYear, maxYear, fieldYear); err != nil {
		return fmt.Errorf("year: %v", err)
	}
	if c.month, err = parseField(parts[1], 1, 12, 0); err != nil {
		return fmt.Errorf("month: %v", err)
	}
	if c.endOfMonth {
		// systemd's limits on days counted from the end: 28 at most (so every
		// month has one), 25 at most within a list, and a lone value's repeat
		// must land on the month's last day or before.
		if c.day, err = parseField(parts[2], 1, 31, fieldFromEnd); err != nil {
			return fmt.Errorf("day: %v", err)
		}
		limit := 28
		distinct := map[item]bool{} // systemd drops duplicates first: "~26,26" is one value
		for _, it := range c.day.items {
			distinct[it] = true
		}
		if len(distinct) > 1 {
			limit = 25
		}
		for _, it := range c.day.items {
			if last := it.lastValue(); it.start > limit || last > limit {
				return fmt.Errorf("day: ~%d is too far from the end of the month (at most %d here)", max(it.start, last), limit)
			}
			if it.rep != 0 && !it.isRange && it.start-it.rep < 1 {
				return fmt.Errorf("day: ~%d/%d never repeats", it.start, it.rep)
			}
		}
		return nil
	}
	if c.day, err = parseField(parts[2], 1, 31, 0); err != nil {
		return fmt.Errorf("day: %v", err)
	}
	return nil
}

func (c *calendar) parseTime(tok string) error {
	parts := strings.Split(tok, ":")
	if len(parts) == 2 {
		parts = append(parts, "00")
	}
	if len(parts) != 3 {
		return fmt.Errorf("a time is HOUR:MINUTE[:SECOND], not %q", tok)
	}
	if strings.Contains(strings.ReplaceAll(parts[2], "..", ""), ".") {
		return fmt.Errorf("fractional seconds aren't supported (the tick runs once a minute)")
	}
	var err error
	if c.hour, err = parseField(parts[0], 0, 23, 0); err != nil {
		return fmt.Errorf("hour: %v", err)
	}
	if c.minute, err = parseField(parts[1], 0, 59, 0); err != nil {
		return fmt.Errorf("minute: %v", err)
	}
	if c.sec, err = parseField(parts[2], 0, 59, fieldSeconds); err != nil {
		return fmt.Errorf("second: %v", err)
	}
	return nil
}

type fieldKind int

const (
	fieldYear    fieldKind = 1 << iota // two-digit years
	fieldSeconds                       // no N..N
	fieldFromEnd                       // the day after "~": its repeat runs the other way
)

// parseField reads one component: "*", or a comma list of N, N..M, N/R and
// N..M/R. "*" takes no repeat.
func parseField(s string, lo, hi int, kind fieldKind) (field, error) {
	isYear := kind&fieldYear != 0
	if s == "*" {
		return field{}, nil
	}
	if s == "" {
		return field{}, errors.New("empty")
	}
	var f field
	for _, part := range strings.Split(s, ",") {
		body, repTxt, hasRep := strings.Cut(part, "/")
		a, b, isRange := strings.Cut(body, "..")
		start, err := num(a, lo, hi, isYear)
		if err != nil {
			return field{}, err
		}
		end := start
		if isRange {
			if end, err = num(b, lo, hi, isYear); err != nil {
				return field{}, err
			}
			if end < start {
				return field{}, fmt.Errorf("range %q runs backwards", body)
			}
			if end == start && kind&fieldSeconds != 0 && !strings.Contains(part, "/") {
				return field{}, fmt.Errorf("seconds range %q is a single value; write it as one", body)
			}
		}
		rep := 0
		if hasRep {
			r, err := strconv.Atoi(repTxt)
			if err != nil || r <= 0 || !allDigits(repTxt) {
				return field{}, fmt.Errorf("repeat %q must be a positive number", repTxt)
			}
			// systemd refuses a single value whose repeat can never fire again
			// ("*:42/60"); a range may repeat past its end.
			if !isRange && kind&fieldFromEnd == 0 && start+r > hi {
				return field{}, fmt.Errorf("%d/%d never repeats (past %d)", start, r, hi)
			}
			rep = r
		}
		f.items = append(f.items, item{start, end, rep, isRange})
	}
	return f, nil
}

func num(s string, lo, hi int, isYear bool) (int, error) {
	if !allDigits(s) {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	// systemd reads a two-digit year as 1970..2069.
	if isYear && len(s) <= 2 {
		if v < 70 {
			v += 2000
		} else {
			v += 1900
		}
	}
	if v < lo || v > hi {
		return 0, fmt.Errorf("%d is outside %d..%d", v, lo, hi)
	}
	return v, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (c *calendar) location() *time.Location {
	if c.loc != nil {
		return c.loc
	}
	return time.Local
}

func (c *calendar) matchDay(y int, m time.Month, d int, wd time.Weekday) bool {
	if !c.year.match(y) || !c.month.match(int(m)) {
		return false
	}
	if c.endOfMonth {
		back := daysIn(y, m) - d + 1
		if !c.day.any() {
			ok := false
			for _, it := range c.day.items {
				if it.matchFromEnd(back) {
					ok = true
					break
				}
			}
			if !ok {
				return false
			}
		}
	} else if !c.day.match(d) {
		return false
	}
	if c.weekdays != 0 {
		idx := (int(wd) + 6) % 7 // Go's Sunday=0 to Mon=0
		if c.weekdays&(1<<idx) == 0 {
			return false
		}
	}
	return true
}

func daysIn(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// next is the first moment strictly after `after` that the spec names, or nil
// if there is none before maxYear runs out.
//
// It walks the wall clock forward from the base's own wall-clock time, in the
// spec's zone, and maps each wall time to an instant the way systemd does: a
// wall time that doesn't exist (the hour skipped in spring) is skipped, and
// one that happens twice (the hour repeated in autumn) is its earliest
// occurrence after the base. So a walk that starts before the repeated hour
// runs through it once, and one that starts inside its second pass carries on
// in that pass.
func (c *calendar) next(after time.Time) *time.Time {
	if c.epoch != nil {
		if c.epoch.After(after) {
			t := *c.epoch
			return &t
		}
		return nil
	}
	loc := c.location()
	start := after.In(loc)
	sy, sm, sd := start.Date()
	startWall := start.Hour()*3600 + start.Minute()*60 + start.Second()
	day := time.Date(sy, sm, sd, 12, 0, 0, 0, loc)
	hours := values(c.hour, 0, 23)
	mins := values(c.minute, 0, 59)
	secs := values(c.sec, 0, 59)
	for ; day.Year() <= maxYear; day = day.AddDate(0, 0, 1) {
		y, m, d := day.Date()
		if !c.year.any() && y > lastValue(c.year) {
			return nil
		}
		if !c.matchDay(y, m, d, day.Weekday()) {
			continue
		}
		first := y == sy && m == sm && d == sd
		for _, h := range hours {
			for _, mi := range mins {
				for _, s := range secs {
					if first && h*3600+mi*60+s <= startWall {
						continue // the walk starts after the base's own wall clock
					}
					if t, ok := wallTimeAfter(y, m, d, h, mi, s, loc, after); ok {
						u := t.UTC()
						return &u
					}
				}
			}
		}
	}
	return nil
}

func lastValue(f field) int {
	hi := 0
	for _, it := range f.items {
		e := it.end
		if it.rep != 0 && !it.isRange {
			e = maxYear
		}
		hi = max(hi, e)
	}
	return hi
}

// values lists, in order, every value in lo..hi that the field matches.
func values(f field, lo, hi int) []int {
	var out []int
	for v := lo; v <= hi; v++ {
		if f.match(v) {
			out = append(out, v)
		}
	}
	return out
}

// wallTimeAfter is the earliest instant after `after` whose wall clock in loc
// reads exactly this, or false if there is none (the hour skipped in spring,
// or every occurrence already past).
func wallTimeAfter(y int, m time.Month, d, h, mi, s int, loc *time.Location, after time.Time) (time.Time, bool) {
	t, ok := wallTime(y, m, d, h, mi, s, loc)
	if !ok {
		return time.Time{}, false
	}
	if t.After(after) {
		return t, true
	}
	if l := t.Add(time.Hour); sameWall(l, y, m, d, h, mi, s) && l.After(after) {
		return l, true // the second occurrence of a repeated wall time
	}
	return time.Time{}, false
}

// wallTime is the first instant whose wall clock in loc reads exactly this, or
// false if no instant does (the hour skipped in spring).
func wallTime(y int, m time.Month, d, h, mi, s int, loc *time.Location) (time.Time, bool) {
	t := time.Date(y, m, d, h, mi, s, 0, loc)
	if !sameWall(t, y, m, d, h, mi, s) {
		return time.Time{}, false
	}
	// time.Date may pick either occurrence of a repeated wall time; take the
	// earlier one if an hour before reads the same.
	if e := t.Add(-time.Hour); sameWall(e, y, m, d, h, mi, s) {
		return e, true
	}
	return t, true
}

func sameWall(t time.Time, y int, m time.Month, d, h, mi, s int) bool {
	ty, tm, td := t.Date()
	return ty == y && tm == m && td == d && t.Hour() == h && t.Minute() == mi && t.Second() == s
}

// normalized prints the spec the way `systemd-analyze calendar` does in its
// "Normalized form" line, so `crontinental check` reads the same.
func (c *calendar) normalized() string {
	if c.epoch != nil {
		return c.epoch.Format("2006-01-02 15:04:05") + " UTC"
	}
	var b strings.Builder
	if c.weekdays != 0 {
		b.WriteString(formatWeekdays(c.weekdays))
		b.WriteString(" ")
	}
	b.WriteString(formatField(c.year, 4))
	b.WriteString("-")
	b.WriteString(formatField(c.month, 2))
	if c.endOfMonth {
		b.WriteString("~")
	} else {
		b.WriteString("-")
	}
	b.WriteString(formatField(c.day, 2))
	b.WriteString(" ")
	b.WriteString(formatField(c.hour, 2))
	b.WriteString(":")
	b.WriteString(formatField(c.minute, 2))
	b.WriteString(":")
	b.WriteString(formatField(c.sec, 2))
	if c.tzName != "" {
		b.WriteString(" ")
		b.WriteString(c.tzName)
	}
	return b.String()
}

func formatWeekdays(mask uint8) string {
	var parts []string
	for d := 0; d < 7; {
		if mask&(1<<d) == 0 {
			d++
			continue
		}
		e := d
		for e+1 < 7 && mask&(1<<(e+1)) != 0 {
			e++
		}
		name := strings.ToUpper(weekdayNames[d][:1]) + weekdayNames[d][1:3]
		switch {
		case e-d >= 2:
			parts = append(parts, name+".."+strings.ToUpper(weekdayNames[e][:1])+weekdayNames[e][1:3])
		default:
			for x := d; x <= e; x++ {
				parts = append(parts, strings.ToUpper(weekdayNames[x][:1])+weekdayNames[x][1:3])
			}
		}
		d = e + 1
	}
	return strings.Join(parts, ",")
}

func formatField(f field, width int) string {
	if f.any() {
		return "*"
	}
	items := append([]item(nil), f.items...)
	sort.SliceStable(items, func(i, j int) bool { return items[i].start < items[j].start })
	var parts []string
	seen := map[item]bool{}
	for _, it := range items {
		if seen[it] {
			continue
		}
		seen[it] = true
		s := fmt.Sprintf("%0*d", width, it.start)
		end := it.lastValue()
		if end != it.start {
			s += fmt.Sprintf("..%0*d", width, end)
		}
		if it.rep != 0 {
			s += fmt.Sprintf("/%d", it.rep)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ",")
}
