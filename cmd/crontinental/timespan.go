package main

// A native implementation of systemd's time span grammar (systemd.time(7),
// "Parsing Time Spans"): "90m", "2h 30min", "1.5h", "1m1" (61s). On Linux the
// tests check it against `systemd-analyze timespan`.

import (
	"errors"
	"fmt"
	"strings"
)

// Microseconds per unit. Case matters: M is a month, m a minute.
var spanUnits = []struct {
	name string
	us   int64
}{
	// Longest names first, so "months" never reads as "m" + "onths".
	{"seconds", 1_000_000}, {"second", 1_000_000}, {"minutes", 60_000_000},
	{"minute", 60_000_000}, {"months", 2_629_800_000_000}, {"month", 2_629_800_000_000},
	{"hours", 3_600_000_000}, {"hour", 3_600_000_000}, {"weeks", 604_800_000_000},
	{"week", 604_800_000_000}, {"years", 31_557_600_000_000}, {"year", 31_557_600_000_000},
	{"days", 86_400_000_000}, {"day", 86_400_000_000}, {"msec", 1_000}, {"usec", 1},
	{"sec", 1_000_000}, {"min", 60_000_000}, {"hr", 3_600_000_000}, {"ms", 1_000},
	{"us", 1}, {"µs", 1}, {"μs", 1}, {"s", 1_000_000}, {"m", 60_000_000},
	{"h", 3_600_000_000}, {"d", 86_400_000_000}, {"w", 604_800_000_000},
	{"M", 2_629_800_000_000}, {"y", 31_557_600_000_000},
}

// parseTimespan is the whole seconds in a span. A bare number is seconds.
func parseTimespan(spec string) (int64, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return 0, errors.New("empty time span")
	}
	var total int64
	for s != "" {
		// a number: digits, optionally a fraction (".5" and "1.5", not "1.")
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		whole := s[:i]
		frac := ""
		if i < len(s) && s[i] == '.' {
			j := i + 1
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			frac = s[i+1 : j]
			if frac == "" {
				return 0, fmt.Errorf("%q is not a time span", spec)
			}
			i = j
		}
		if whole == "" && frac == "" {
			return 0, fmt.Errorf("%q is not a time span", spec)
		}
		s = strings.TrimLeft(s[i:], " ")

		unit := int64(1_000_000) // no unit: seconds
		for _, u := range spanUnits {
			if strings.HasPrefix(s, u.name) {
				rest := s[len(u.name):]
				// the unit must end here: "5mx" isn't minutes
				if rest == "" || rest[0] == ' ' || (rest[0] >= '0' && rest[0] <= '9') || rest[0] == '.' {
					unit = u.us
					s = strings.TrimLeft(rest, " ")
					goto found
				}
			}
		}
		if s != "" && !(s[0] >= '0' && s[0] <= '9') && s[0] != '.' {
			return 0, fmt.Errorf("%q is not a time span (unknown unit in %q)", spec, s)
		}
	found:
		var w int64
		for _, c := range whole {
			w = w*10 + int64(c-'0')
		}
		total += w * unit
		// each fraction digit is worth a tenth of the last, truncating as systemd does
		k := unit
		for _, c := range frac {
			k /= 10
			total += int64(c-'0') * k
		}
	}
	return total / 1_000_000, nil
}
