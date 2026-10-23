package main

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Random specs built from the grammar's pieces, checked against systemd. The
// seed is fixed so a failure reproduces; CRONTINENTAL_FUZZ=N runs N cases
// instead of the default few hundred.
func TestCalendarRandomAgainstSystemd(t *testing.T) {
	haveTool(t, "systemd-analyze", "calendar", "daily")
	n := 300
	if v, err := strconv.Atoi(os.Getenv("CRONTINENTAL_FUZZ")); err == nil {
		n = v
	}
	r := rand.New(rand.NewSource(20261023))
	comp := func(lo, hi int, width int) string {
		pick := func() int { return lo + r.Intn(hi-lo+1) }
		pad := func(v int) string { return fmt.Sprintf("%0*d", width, v) }
		switch r.Intn(7) {
		case 0, 1:
			return "*"
		case 2:
			return pad(pick())
		case 3:
			a, b := pick(), pick()
			if a > b {
				a, b = b, a
			}
			return pad(a) + ".." + pad(b)
		case 4:
			return pad(pick()) + "/" + strconv.Itoa(1+r.Intn(hi-lo+1))
		case 5:
			a, b := pick(), pick()
			if a > b {
				a, b = b, a
			}
			return pad(a) + ".." + pad(b) + "/" + strconv.Itoa(1+r.Intn(5))
		default:
			return pad(pick()) + "," + pad(pick())
		}
	}
	days := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	mismatches := 0
	for i := 0; i < n && mismatches < 20; i++ {
		spec := ""
		if r.Intn(3) == 0 {
			a := r.Intn(7)
			b := a + r.Intn(7-a)
			if a == b {
				spec = days[a] + " "
			} else {
				spec = days[a] + ".." + days[b] + " "
			}
		}
		if r.Intn(4) != 0 {
			year := "*"
			if r.Intn(4) == 0 {
				year = comp(2026, 2030, 4)
			}
			sep := "-"
			if r.Intn(6) == 0 {
				sep = "~"
			}
			spec += year + "-" + comp(1, 12, 2) + sep + comp(1, 31, 2) + " "
		}
		spec += comp(0, 23, 2) + ":" + comp(0, 59, 2)
		if r.Intn(2) == 0 {
			spec += ":" + comp(0, 59, 2)
		}
		zone := zones[r.Intn(len(zones))]
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(r.Int63n(int64(3 * 365 * 24 * time.Hour))))
		bs := base.Format("2006-01-02 15:04:05") + " UTC"

		want, _, never, ok := systemdNext(spec, zone, bs)
		c, perr := parseCalendar(spec)
		label := fmt.Sprintf("%q TZ=%s base=%s", spec, zone, bs)
		switch {
		case !ok && perr == nil:
			t.Errorf("%s: systemd refuses, we accept", label)
			mismatches++
			continue
		case ok && perr != nil:
			t.Errorf("%s: systemd accepts, we refuse: %v", label, perr)
			mismatches++
			continue
		case !ok:
			continue
		}
		if !never && want.IsZero() {
			continue // systemd gave up; see systemdNext
		}
		c.loc, _ = time.LoadLocation(zone)
		got := c.next(base)
		if never != (got == nil) || (!never && !got.Equal(want)) {
			// Ask again with every repeat written out as a list. systemd's
			// repeat handling has bugs ("01/13:00" skips 01:00 once the search
			// rolls over a day; "01,14:00" doesn't); if its own answer for the
			// list form is ours, the disagreement is systemd's.
			if lst, ok := c.listForm(); ok {
				w2, _, n2, ok2 := systemdNext(lst, zone, bs)
				if ok2 && n2 == (got == nil) && (got == nil || got.Equal(w2)) {
					t.Logf("%s: systemd's repeat bug (it answers %v; as %q it agrees with us)", label, want, lst)
					continue
				}
			}
			t.Errorf("%s: systemd %v (never=%v), we %v", label, want, never, got)
			mismatches++
		}
	}
}

// listForm writes the spec with every repeat and range spelled out as a plain
// list, which steers clear of systemd's repeat bugs. Not for "~" specs (lists
// there have tighter limits) or open-ended year repeats.
func (c *calendar) listForm() (string, bool) {
	if c.endOfMonth || c.epoch != nil {
		return "", false
	}
	list := func(f field, lo, hi, width int) string {
		if f.any() {
			return "*"
		}
		var parts []string
		for _, v := range values(f, lo, hi) {
			parts = append(parts, fmt.Sprintf("%0*d", width, v))
		}
		return strings.Join(parts, ",")
	}
	year := "*"
	if !c.year.any() {
		year = list(c.year, minYear, maxYear, 4)
		if strings.Count(year, ",") > 40 {
			return "", false
		}
	}
	spec := ""
	if c.weekdays != 0 {
		spec = formatWeekdays(c.weekdays) + " "
	}
	spec += year + "-" + list(c.month, 1, 12, 2) + "-" + list(c.day, 1, 31, 2) + " " +
		list(c.hour, 0, 23, 2) + ":" + list(c.minute, 0, 59, 2) + ":" + list(c.sec, 0, 59, 2)
	return spec, true
}
