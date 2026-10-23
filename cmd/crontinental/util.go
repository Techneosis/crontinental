package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	unitPrefix        = "crontinental-job-"
	glyph             = "󰃰"
	defaultTimeoutSec = 3600

	// A run that fires within this many seconds of its due time is "on time".
	// The tick is 1/min, so an on-time fire is always under 60s late; anything
	// past this means the machine was asleep, off, or wedged -- which is exactly
	// the case --catchup decides.
	graceSec = 120

	// The exit status a command uses to say "my work is finished, stop running
	// me". Not 100: apt-get exits 100 on error, and a failing job must never
	// look done.
	doneExit = 99

	// How long a finished job stays around for show/log before the tick
	// deletes it.
	retainSec = 3 * 86400
)

// isoLayout is what the ledger stores: Python's datetime.isoformat() for a
// whole-second UTC time ("2026-10-23T08:25:57+00:00"). The tick compares these
// as strings, so every writer must produce exactly this shape.
const isoLayout = "2006-01-02T15:04:05-07:00"

func now() time.Time { return time.Now().UTC().Truncate(time.Second) }

func iso(t time.Time) string { return t.UTC().Format(isoLayout) }

func isoOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return iso(*t)
}

func parseISO(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		die("ledger holds an unreadable time %q", s)
	}
	return t
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "crontinental: "+format+"\n", args...)
	os.Exit(1)
}

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		die("cannot find the home directory: %v", err)
	}
	return h
}

func humanize(t *time.Time) string {
	if t == nil {
		return "-"
	}
	local := t.Local().Format("Mon 01-02 15:04")
	delta := t.Sub(now()).Seconds()
	if math.Abs(delta) < 5 {
		return local + " (just now)"
	}
	future := delta >= 0
	if !future {
		delta = -delta
	}
	var n string
	switch {
	case delta < 90:
		n = fmt.Sprintf("%ds", int(delta))
	case delta < 5400:
		n = fmt.Sprintf("%dm", int(delta/60))
	case delta < 172800:
		n = fmt.Sprintf("%.1fh", delta/3600)
	default:
		n = fmt.Sprintf("%dd", int(delta/86400))
	}
	if future {
		return fmt.Sprintf("%s (in %s)", local, n)
	}
	return fmt.Sprintf("%s (%s ago)", local, n)
}

func timePtr(s string) *time.Time {
	t := parseISO(s)
	return &t
}

func describeEnd(j *Job) string {
	if j.Kind == "once" {
		return "when it fires"
	}
	if j.Forever != 0 {
		return "never (--forever)"
	}
	var parts []string
	if j.Until.Valid {
		parts = append(parts, "at "+humanize(timePtr(j.Until.String)))
	}
	if j.UntilDone != 0 {
		parts = append(parts, fmt.Sprintf("when the command exits %d", doneExit))
	}
	if len(parts) == 0 {
		return "never (scheduled before ends were required)"
	}
	return strings.Join(parts, ", or ")
}

func unitName(name string) string { return unitPrefix + name + ".service" }

// logIdent tags a job's output in the journal (see dispatch).
func logIdent(name string) string { return unitPrefix + name }

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,48}$`)

func validName(name string) string {
	if !nameRe.MatchString(name) {
		die("bad job name %q: lowercase letters, digits, - and _ only "+
			"(it becomes a systemd unit name)", name)
	}
	return name
}

var unsafeNameChars = regexp.MustCompile(`[^a-z0-9_-]+`)

func defaultName(command string) string {
	base := "job"
	if f := strings.Fields(command); len(f) > 0 {
		base = filepath.Base(strings.Trim(f[0], `'"`))
	}
	base = strings.Trim(unsafeNameChars.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if base == "" {
		base = "job"
	}
	return base + "-" + now().Format("0102150405") // UTC, as the Python version did
}

func provenance() string {
	if sid := os.Getenv("CLAUDE_CODE_SESSION_ID"); sid != "" {
		if len(sid) > 8 {
			sid = sid[:8]
		}
		return "claude:" + sid
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}

func notify(headline, body string, critical bool) {
	urgency := "low"
	if critical {
		urgency = "critical"
	}
	// Omarchy's own sender where it exists: it carries the glyph and, unlike a
	// bare notify-send there, survives the desktop's notification changes.
	// Everywhere else the freedesktop standard.
	var argv []string
	if p, err := exec.LookPath("omarchy-notification-send"); err == nil {
		argv = []string{p, "-g", glyph, "-u", urgency, headline, body}
	} else if p, err := exec.LookPath("notify-send"); err == nil {
		argv = []string{p, "-a", "crontinental", "-u", urgency, headline, body}
	} else {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, argv[0], argv[1:]...).Run() // a missing notifier must never fail a job
}

func runTool(name string, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.Command(name, args...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return []byte(out.String()), []byte(errb.String()), err
}

// joinCommand: one argument after `--` is a shell line, verbatim, so pipes and
// && survive. Several are an argv, quoted safely. Both end up as a string for
// the login shell.
func joinCommand(parts []string) string {
	if len(parts) == 1 {
		return parts[0]
	}
	q := make([]string, len(parts))
	for i, p := range parts {
		q[i] = shellQuote(p)
	}
	return strings.Join(q, " ")
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote matches Python's shlex.quote, so a command stored by either
// implementation reads the same.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// truncate cuts to n characters, not bytes, so a multi-byte character is never split.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
