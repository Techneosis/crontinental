package main

import (
	"time"
)

// The three grammars (calendar.go, timespan.go, when.go) are native, so a
// schedule means the same thing on every platform. The tests check them
// against systemd-analyze and GNU date wherever those exist.

// calendarNext is the next elapse of a calendar spec strictly after `after`
// (or now), nil if it never elapses again. An error means the spec is not a
// calendar expression at all, which is how `every 30m` falls through to the
// interval reading.
func calendarNext(spec string, after *time.Time) (*time.Time, error) {
	c, err := parseCalendar(spec)
	if err != nil {
		return nil, err
	}
	base := now()
	if after != nil {
		base = *after
	}
	return c.next(base), nil
}

// parseWhen is an absolute time, as a person types it, in local time.
func parseWhen(spec string) time.Time {
	t, err := parseWhenAt(spec, now(), time.Local)
	if err != nil {
		die("%v", err)
	}
	return t
}

// nextFire is when a repeat should next run, measured from `after` -- never
// from the due time it missed. A box that was asleep for a day therefore wakes
// owing at most one run of each job, not a day's worth queued up behind each
// other.
func nextFire(j *Job, after time.Time) (*time.Time, error) {
	if j.IntervalSec.Valid && j.IntervalSec.Int64 != 0 {
		t := after.Add(time.Duration(j.IntervalSec.Int64) * time.Second)
		return &t, nil
	}
	return calendarNext(j.Spec.String, &after)
}
