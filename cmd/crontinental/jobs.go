package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------- add

func cmdAdd(o *opts, kind string) {
	command := joinCommand(o.command)
	if command == "" {
		die("no command given -- put it after `--`")
	}
	name := defaultName(command)
	if o.name != "" {
		name = validName(o.name)
	}

	db := connect()
	var exists int
	if db.QueryRow("SELECT 1 FROM job WHERE name=?", name).Scan(&exists) == nil {
		die("a job named %q already exists (rm it, or pass a different --name)", name)
	}

	var (
		when           time.Time
		interval       *int64
		spec           *string
		defaultCatchup string
	)
	if kind == "once" {
		if o.verb == "at" {
			when = parseWhen(o.when)
		} else {
			secs, err := parseTimespan(o.when)
			if err != nil {
				die("%q is not a delay: %v (try 90m, 2h, 1d)", o.when, err)
			}
			when = now().Add(time.Duration(secs) * time.Second)
		}
		if !when.After(now()) {
			die("that time is in the past: %s", when.Local().Format("2006-01-02 15:04"))
		}
		defaultCatchup = "run"
	} else {
		s := o.when
		spec = &s
		next, iv := parseRepeat(s)
		when = next
		if secs, ok := iv.(int64); ok {
			interval = &secs
		}
		// systemd's own default for a missed timer run is to skip it, and the
		// same reasoning holds here: a nightly job that wakes at noon usually
		// wants tonight, not a surprise run in the middle of the day.
		defaultCatchup = "skip"
	}

	var until *time.Time
	if o.until != "" {
		u := parseWhen(o.until)
		until = &u
		if !u.After(now()) {
			die("--until is in the past: %s", u.Local().Format("2006-01-02 15:04"))
		}
	}
	if kind == "repeat" {
		if o.forever && (until != nil || o.untilDone) {
			die("--forever contradicts --until/--until-done; pick one")
		}
		if until == nil && !o.untilDone && !o.forever {
			die("a repeat has to say how it ends:\n"+
				"  --until <time>   stop after a deadline (\"tomorrow 18:00\", \"3 days\")\n"+
				"  --until-done     stop when the command exits %d (polling for something)\n"+
				"  --forever        never stop -- only for jobs meant to be permanent", doneExit)
		}
	} else if o.forever || o.untilDone {
		die("--forever/--until-done are for repeats; a one-off ends when it fires")
	}

	var workdir *string
	if o.workdir != "" {
		w := resolveDir(o.workdir)
		if st, err := os.Stat(w); err != nil || !st.IsDir() {
			die("--workdir is not a directory: %s", w)
		}
		workdir = &w
	}

	timeout := int64(defaultTimeoutSec)
	if o.timeout != "" {
		secs, err := parseTimespan(o.timeout)
		if err != nil {
			die("%q is not a timeout: %v", o.timeout, err)
		}
		timeout = secs
	}
	catchup := o.catchup
	if catchup == "" {
		catchup = defaultCatchup
	}
	by := o.by
	if by == "" {
		by = provenance()
	}

	mustExec(db, `INSERT INTO job (name, command, workdir, kind, spec, interval_sec, next_run,
		catchup, until, until_done, forever, timeout_sec, notify, note, created_at, created_by)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		name, command, workdir, kind, spec, interval, iso(when),
		catchup, isoOrNil(until), b2i(o.untilDone), b2i(o.forever), timeout,
		o.notify, nullIfEmpty(o.note), iso(now()), by)

	fmt.Printf("%s: next run %s\n", name, humanize(&when))
	if kind == "repeat" {
		if interval != nil {
			fmt.Printf("  repeating: every %s\n", *spec)
		} else {
			fmt.Printf("  repeating: %s\n", *spec)
		}
		fmt.Printf("  ends: %s\n", describeEnd(getJob(db, name)))
	}
	fmt.Printf("  command: %s\n", command)
}

// resolveDir is ~-expansion plus an absolute, symlink-free path, like
// Python's Path.expanduser().resolve() (which tolerates a missing path).
func resolveDir(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = filepath.Join(homeDir(), p[1:])
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --------------------------------------------------------------------- tick

func cmdTick() {
	db := connect()
	t := now()
	reap(db, t)

	rows, err := db.Query("SELECT "+jobCols+" FROM job WHERE enabled=1 AND next_run IS NOT NULL"+
		" AND next_run<=? ORDER BY next_run", iso(t))
	if err != nil {
		die("ledger: %v", err)
	}
	var due []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			die("ledger: %v", err)
		}
		due = append(due, j)
	}
	rows.Close()

	for _, j := range due {
		dueAt := parseISO(j.NextRun.String)
		late := int64(t.Sub(dueAt).Seconds())

		if j.Until.Valid && !t.Before(parseISO(j.Until.String)) {
			finishJob(db, j.ID)
			continue
		}

		// Advance BEFORE dispatching. If anything below fails, the job has
		// already moved on -- the alternative is a job that failed to launch
		// being retried every 60 seconds forever.
		if err := advance(db, j, t); err != nil {
			fmt.Fprintf(os.Stderr, "crontinental: %s: cannot compute its next run: %v\n", j.Name, err)
			continue
		}

		if j.Catchup == "skip" && late > graceSec {
			record(db, j, dueAt, late, "skipped", fmt.Sprintf("%ds late; catchup=skip", late), "")
			continue
		}
		dispatch(db, j, dueAt, late)
	}
}

func advance(db *sql.DB, j *Job, t time.Time) error {
	var next *time.Time
	if j.Kind == "repeat" {
		n, err := nextFire(j, t)
		if err != nil {
			return err
		}
		next = n
		if next != nil && j.Until.Valid && !next.Before(parseISO(j.Until.String)) {
			next = nil
		}
	}
	if next == nil {
		finishJob(db, j.ID)
	} else {
		mustExec(db, "UPDATE job SET next_run=? WHERE id=?", iso(*next), j.ID)
	}
	return nil
}

// reap deletes jobs finished more than retainSec ago (their runs cascade). A
// run still marked running holds its job back, so a long last run is never
// pulled out from under its own `exec` -- unless that row is older than
// retainSec, which only an `exec` killed too hard to write down its ending
// leaves behind.
func reap(db *sql.DB, t time.Time) {
	cutoff := iso(t.Add(-retainSec * time.Second))
	mustExec(db, `DELETE FROM job WHERE finished_at IS NOT NULL AND finished_at<? AND NOT EXISTS
		(SELECT 1 FROM run WHERE run.job_id=job.id AND outcome='running' AND started_at>=?)`,
		cutoff, cutoff)
}

func record(db *sql.DB, j *Job, dueAt time.Time, late int64, outcome, detail, unit string) {
	n := iso(now())
	mustExec(db, `INSERT INTO run (job_id, unit, due_at, started_at, finished_at, outcome, late_sec, detail)
		VALUES (?,?,?,?,?,?,?,?)`,
		j.ID, nullIfEmpty(unit), iso(dueAt), n, n, outcome, late, nullIfEmpty(detail))
}

func self() string {
	p, err := os.Executable()
	if err != nil {
		die("cannot find my own binary: %v", err)
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

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

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// --------------------------------------------------------------------- exec

// cmdExec is what the transient unit runs. The command's own output streams
// straight to the journal (no capture), so `log` has one source of truth.
func cmdExec(o *opts) {
	db := connect()
	j := getJob(db, o.name)
	unit := unitName(j.Name)
	dueAt := now()
	if o.due != "" {
		dueAt = parseISO(o.due)
	}
	started := now()

	res := mustExec(db, `INSERT INTO run (job_id, unit, due_at, started_at, outcome, late_sec)
		VALUES (?,?,?,?, 'running', ?)`, j.ID, unit, iso(dueAt), iso(started), o.late)
	runID, _ := res.LastInsertId()

	finish := func(status int, outcome, detail string) {
		mustExec(db, "UPDATE run SET finished_at=?, status=?, outcome=?, detail=? WHERE id=?",
			iso(now()), status, outcome, nullIfEmpty(detail), runID)
	}

	// RuntimeMaxSec expired, or someone stopped the unit. systemd kills the
	// whole cgroup, so the child is already going; all that is left is to say
	// so before this process goes too.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	go func() {
		sig := (<-sigs).(syscall.Signal)
		finish(128+int(sig), "killed", fmt.Sprintf("stopped by signal %d", int(sig)))
		if j.Notify != "never" {
			notify("Scheduled job killed", fmt.Sprintf("%s hit its %ds limit", j.Name, j.TimeoutSec), true)
		}
		os.Exit(128 + int(sig))
	}()

	workdir := homeDir()
	if j.Workdir.Valid {
		workdir = j.Workdir.String
	}
	cmd := exec.Command("/bin/bash", "-lc", j.Command)
	cmd.Dir = workdir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, os.Stdout, os.Stderr
	err := cmd.Run()

	status := 0
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			status = -int(ws.Signal()) // as Python reports a child killed by a signal
		} else {
			status = exitErr.ExitCode()
		}
	default: // the command could not be started at all; the unit must still record why
		finish(-1, "error", truncate(err.Error(), 500))
		if j.Notify != "never" {
			notify("Scheduled job errored", truncate(j.Name+": "+err.Error(), 160), true)
		}
		os.Exit(1)
	}

	elapsed := int(now().Sub(started).Seconds())
	if status == doneExit {
		// The job's own verdict that there is nothing left to do. Honoured on
		// every job, declared or not: a command that says it is finished and
		// keeps getting run is worse than one that stops a little early.
		finish(status, "done", "")
		finishJob(db, j.ID)
		if j.Notify == "always" {
			notify("Scheduled job finished", fmt.Sprintf("%s reported done after %ds", j.Name, elapsed), false)
		}
		os.Exit(0) // done is success; the unit should not show as failed
	}

	outcome := "ok"
	if status != 0 {
		outcome = "fail"
	}
	finish(status, outcome, "")
	if status != 0 && (j.Notify == "fail" || j.Notify == "always") {
		notify("Scheduled job failed", fmt.Sprintf("%s exited %d after %ds — crontinental log %s",
			j.Name, status, elapsed, j.Name), true)
	} else if status == 0 && j.Notify == "always" {
		notify("Scheduled job done", fmt.Sprintf("%s finished in %ds", j.Name, elapsed), false)
	}
	os.Exit(status & 0xff) // the unit's own state should mirror the job's
}
