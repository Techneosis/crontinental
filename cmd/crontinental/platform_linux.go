//go:build linux

package main

// Linux: systemd does the heavy lifting. The tick is a user timer, each run is
// a transient unit (its own cgroup, journal stream and RuntimeMaxSec), and
// systemd refusing a second unit of the same name is the overlap guard.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	unitPrefix = "crontinental-job-"
	glyph      = "󰃰"

	// selfManaged: does exec itself enforce the timeout and stop the command?
	// Here systemd does both (RuntimeMaxSec, and the cgroup kill on stop).
	selfManaged = false
)

func unitName(name string) string { return unitPrefix + name + ".service" }

// logIdent tags a job's output in the journal (see dispatch).
func logIdent(name string) string { return unitPrefix + name }

// runRef is what a run row records as where the run happened.
func runRef(name string) string { return unitName(name) }

func runnerInfo(name string) (label, where string) { return "unit", unitName(name) }

func shellArgv(command string) []string { return []string{"/bin/bash", "-lc", command} }

func shellDesc() string { return "login shell, bash -lc" }

// The journal stamps every line itself, so a run needs no banner.
func banner(string) {}

// No lock: systemd refuses a second unit of the same name (see dispatch).
type runGuard struct{}

func guardRun(string) (runGuard, bool) { return runGuard{}, true }
func (runGuard) started(int)           {}

func dispatch(db *sql.DB, j *Job, dueAt time.Time, late int64) {
	unit := unitName(j.Name)
	args := []string{"--user", "--no-block", "--collect", "--unit=" + unit,
		"--description=scheduled job: " + j.Name,
		fmt.Sprintf("--property=RuntimeMaxSec=%d", j.TimeoutSec),
		// Tag the output with the job, in the stream itself. journald otherwise
		// attributes stdout to a unit by looking up the writing process, and a
		// job that prints and exits at once can be gone before it looks: the line
		// is kept but never shows under -u. The identifier cannot lose that race.
		"--property=SyslogIdentifier=" + logIdent(j.Name)}
	// systemd-run starts the unit from the user manager's environment, not this
	// process's, so a CRONTINENTAL_DB override has to be handed over explicitly
	// -- otherwise a test tick dispatches jobs that record into the real ledger.
	if v, ok := os.LookupEnv("CRONTINENTAL_DB"); ok {
		args = append(args, "--setenv=CRONTINENTAL_DB="+v)
	}
	// The unit runs `exec`, not the command itself: something has to outlive
	// the command to write down what it did, and this is the only process
	// inside the job's own cgroup that can.
	args = append(args, self(), "exec", j.Name, "--due", iso(dueAt), "--late", strconv.FormatInt(late, 10))

	_, errb, err := runTool("systemd-run", args...)
	if err == nil {
		return
	}
	msg := strings.TrimSpace(string(errb))
	// The transient unit name is the job name, so systemd itself refuses a
	// second copy of a job that is still running. That refusal IS the overlap
	// guard -- there is no lockfile to go stale. systemd words it several ways
	// depending on how far the old unit got; all of them mean the same thing.
	low := strings.ToLower(msg)
	overlap := strings.Contains(low, "already") && containsAny(low, "loaded", "exists", "running", "active")
	outcome := "error"
	if overlap {
		outcome = "overlap"
	}
	record(db, j, dueAt, late, outcome, truncate(msg, 500), unit)
	if !overlap && j.Notify != "never" {
		notify("Scheduled job could not start", j.Name+": "+truncate(msg, 160), true)
	}
}

func stopRun(name string) {
	_ = exec.Command("systemctl", "--user", "stop", unitName(name)).Run()
}

func showLog(name string, lines int, follow bool) {
	// systemd's own lines for the unit, OR the job's tagged output. The unit
	// matches also find runs logged before output was tagged.
	u := unitName(name)
	args := []string{"--user", "-n", strconv.Itoa(lines), "--no-pager",
		"_SYSTEMD_USER_UNIT=" + u, "+", "USER_UNIT=" + u, "+", "SYSLOG_IDENTIFIER=" + logIdent(name)}
	if follow {
		args = append(args, "-f")
	}
	cmd := exec.Command("journalctl", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	_ = cmd.Run()
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

