package main

import (
	"errors"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// For now every grammar is systemd's or GNU date's, exactly as in the Python
// version: same answers as every unit on this machine, and nothing hand-rolled
// to disagree with them. Replacing these with native parsers (for macOS) is a
// separate step, checked against these tools.

var (
	neverRe = regexp.MustCompile(`Next elapse:\s*never`)
	// systemd prints "(in UTC):" only when local time isn't UTC. On a UTC
	// machine (most servers, every CI runner) "Next elapse:" already is UTC.
	nextUTCRe    = regexp.MustCompile(`\(in UTC\):\s*\w{3} (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) UTC`)
	nextElapseRe = regexp.MustCompile(`Next elapse:\s*\w{3} (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) UTC`)
	timespanRe   = regexp.MustCompile(`(?:μs|us):\s*(\d+)`)
)

func firstLine(b []byte, fallback string) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return fallback
	}
	return strings.SplitN(s, "\n", 2)[0]
}

func runTool(name string, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.Command(name, args...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return []byte(out.String()), []byte(errb.String()), err
}

// systemdCalendar is the next elapse of an OnCalendar spec after `after` (or
// now), nil if it never elapses again. An error means the spec is not a
// calendar expression at all, which is how `every 30m` falls through to the
// timespan reading.
func systemdCalendar(spec string, after *time.Time) (*time.Time, error) {
	args := []string{"calendar", "--iterations=1"}
	if after != nil {
		// --base-time takes a local/absolute stamp; feed it UTC explicitly so a
		// DST boundary cannot shift the answer by an hour.
		args = append(args, "--base-time", after.UTC().Format("2006-01-02 15:04:05")+" UTC")
	}
	out, errb, err := runTool("systemd-analyze", append(args, spec)...)
	if err != nil {
		return nil, errors.New(firstLine(errb, "invalid calendar spec"))
	}
	if neverRe.Match(out) {
		return nil, nil
	}
	m := nextUTCRe.FindSubmatch(out)
	if m == nil {
		m = nextElapseRe.FindSubmatch(out)
	}
	// Unreadable output is an error, never "no next run": reporting a spec as
	// never elapsing is how this used to fail on UTC machines.
	if m == nil {
		return nil, errors.New("could not read systemd-analyze's answer: " + firstLine(out, "(empty)"))
	}
	t, err := time.Parse("2006-01-02 15:04:05", string(m[1]))
	if err != nil {
		return nil, errors.New("could not read systemd-analyze's answer: " + string(m[1]))
	}
	return &t, nil
}

// systemdTimespan is the seconds in a systemd timespan ("90m", "2h 30min", "1d").
func systemdTimespan(spec string) (int64, error) {
	out, errb, err := runTool("systemd-analyze", "timespan", spec)
	if err != nil {
		return 0, errors.New(firstLine(errb, "invalid timespan"))
	}
	m := timespanRe.FindSubmatch(out)
	if m == nil {
		return 0, errors.New("could not read timespan")
	}
	us, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil {
		return 0, errors.New("could not read timespan")
	}
	return us / 1_000_000, nil
}

// parseWhen is an absolute time, as a human types it. `date -d` because it
// takes what a human types.
func parseWhen(spec string) time.Time {
	out, _, err := runTool("date", "-d", spec, "+%s")
	if err != nil {
		die("could not understand the time %q (try \"2026-09-15 09:00\" or \"tomorrow 07:00\")", spec)
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		die("could not understand the time %q", spec)
	}
	return time.Unix(secs, 0).UTC()
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
	return systemdCalendar(j.Spec.String, &after)
}
