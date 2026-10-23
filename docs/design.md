# techneosis-ai-schedule

Run a command later — once, or on a repeat — without creating a systemd unit
for it. Built so an agent can schedule work mid-session and have it survive the
session, the logout, and the reboot.

```
techneosis-ai-schedule in 90m  --name pr-recheck -- gh pr checks 412
techneosis-ai-schedule at "tomorrow 07:00" --name thaw -- ~/bin/thaw-staging
techneosis-ai-schedule every "Mon..Fri 07:30" --name standup --forever -- techneosis-sb agenda
techneosis-ai-schedule every 30m --name health --until "2 hours" -- 'curl -sf https://x/health || exit 1'
techneosis-ai-schedule every 10m --name deploy-wait --until-done --until "tomorrow" -- 'deploy-is-live && exit 99'
```

## Why not just `systemd-run --on-calendar`?

Because it forgets. `systemd-run` creates a *transient* timer, and transient
units die with the user manager — so a job scheduled for tomorrow morning
evaporates at the next reboot, silently, with nothing to show that it was ever
scheduled. On Arch that is not a rare event.

So the three concerns are split deliberately:

| concern | owner |
|---|---|
| durability | the job table (`~/.local/share/techneosis-schedule/schedule.db`) |
| heartbeat | one shared timer, `techneosis-ai-schedule.timer`, 1/min |
| execution | a transient unit per run, via `systemd-run` |

The tick never *runs* a job. It reads the table, finds what is due, and launches
each one as `techneosis-ai-job-<name>.service`, then returns — measured at
~44ms. That is what buys per-job cgroups, per-job journal streams, and per-job
timeouts without a single per-job file, and what guarantees one wedged job can
neither delay the tick nor starve another job.

## The command

Everything after `--` is the command.

- **One argument** is taken as a shell line, verbatim — pipes, `&&`, redirects
  and `$(…)` all work: `-- 'make build 2>&1 | tail -40'`
- **Several arguments** are taken as an argv and quoted safely:
  `-- gh pr checks 412`

Every job runs under `bash -lc`, so its PATH is the login shell's — node, mise,
`~/.local/bin`, `dotfiles/scripts`. This is not cosmetic: systemd's own PATH has
none of those, which is the trap that makes a node-touching command work by hand
and fail only when scheduled (the same one documented in
`techneosis-ai-dispatch-triage.service`).

## Schedules

`at` takes anything `date -d` understands (`"2026-09-15 09:00"`, `"tomorrow
07:00"`, `"next friday 18:00"`). `in` takes a systemd timespan (`90m`, `2h`,
`1d`).

`every` takes systemd's own calendar grammar, parsed by `systemd-analyze` rather
than a hand-rolled cron parser — so it is the same grammar as every unit on this
box, and `systemd-analyze calendar "<spec>"` is the way to check one:

```
03:00               daily at 3am
Mon..Fri 07:30      weekday mornings
*-*-01 09:00        the 1st of every month
hourly              every hour on the hour
```

**`daily 03:00` is not valid** — `daily` already means midnight and takes no
time. Write `03:00`.

`every` also accepts a plain interval (`30m`, `4h`), which repeats that long
after each run rather than landing on wall-clock boundaries. Anything under one
minute is refused: the tick is the resolution.

## Every job ends

A one-off ends when it fires. **A repeat has to say how it ends** when it is
created, or it is refused:

| flag | ends |
|---|---|
| `--until <time>` | at a deadline — anything `date -d` reads (`"tomorrow 18:00"`, `"3 days"`) |
| `--until-done` | when the command exits **99**: the job itself decides its work is finished |
| `--forever` | never — the only way to get a permanent job, so it is always a choice |

`--until` and `--until-done` combine (stop at whichever comes first), which is
the right shape for polling: "until it's live, but give up tomorrow".
`--forever` contradicts both.

Exit 99 is honoured on every job, declared or not, and counts as success: the
run is recorded as `done`, nothing notifies (except `--notify always`), and the
unit exits 0. Not 100 — apt-get exits 100 on error, and a failing job must
never look finished.

This exists because agents schedule things and then their session ends. Before
it, the way to make a repeat stop was a second one-off whose command was
`techneosis-ai-schedule rm <first>`; now the end is part of the job.

Jobs that existed before ends were required were marked `--forever` by the
schema migration, so nothing that was running stopped.

## What happens when the box was asleep

Due times live in the table, so a tick missed while suspended is not a lost job
— the first tick after resume finds everything that came due. Each job then
applies its own `--catchup`:

- **`run`** (default for one-offs) — fire it anyway, late. What you want from
  "remind me at 09:00" when the lid opened at 09:40.
- **`skip`** (default for repeats) — let it go, wait for the next slot. What you
  want from a nightly job when the machine wakes at noon. This matches systemd's
  own default for a missed timer (`Persistent=false`).

A fire is "late" past 120s; under that it is just an on-time fire landing inside
its tick. Either way a repeat reschedules forward from **now**, never from the
slot it missed — so a day asleep costs at most one run of each job, never a
backlog queued up behind itself.

## Overlap, timeouts, failure

**Overlap is impossible by construction.** A run's transient unit is named after
the job, so systemd itself refuses a second copy while the first is running.
There is no lockfile to go stale. The refusal is recorded as an `overlap` run.

**Every job has a timeout**, 1h by default, `--timeout 20m` to change it. It is
enforced as `RuntimeMaxSec` on the transient unit, and the kill is caught and
written down (`killed`, exit 143) rather than leaving a run row open forever.

**Failures notify** (`omarchy-notification-send`, critical) with the job name,
its exit code, and the command to read its log. `--notify always` also announces
successes; `--notify never` stays quiet.

## Reading what happened

```
techneosis-ai-schedule list          what is scheduled, next run, last outcome
techneosis-ai-schedule list --all    including finished and disabled jobs
techneosis-ai-schedule show <name>   one job in full, with its last 10 runs
techneosis-ai-schedule log <name>    its output, from the journal (-f to follow)
techneosis-ai-schedule run <name>    fire it now WITHOUT moving its schedule
```

A job's output is never captured into the database — it streams to the journal
under the job's own unit, so `log` has exactly one source of truth and a chatty
job doesn't bloat the table.

`show` also prints who scheduled it and why: `created_by` is filled in
automatically from `CLAUDE_CODE_SESSION_ID` when an agent schedules something,
so a job nobody remembers can be traced back to the session that wanted it.
That matters here — this is a persistent execution channel that agents can write
to, and `list` is the audit of it.

## Housekeeping

A job tears itself down. When it can never run again — a one-off fired, a
deadline passed, or the command exited 99 — it leaves `list` at once (`list
--all` shows it as `done`, `show` says when it will go) and the tick deletes it,
with its run history, **3 days later**. Its output stays in the journal after
that. A run still in progress holds its job back.

`enable` on a finished repeat takes it off that clock. `disable` is different
from finishing: a disabled job is kept until someone removes it.
`prune [--days N]` deletes finished jobs now instead of waiting.
`rm <name>` stops a running instance and deletes the job.

## Files

- `scripts/techneosis-ai-schedule` — the whole thing
- `systemd/.config/systemd/user/techneosis-ai-schedule.{timer,service}` — the tick
- `~/.local/share/techneosis-schedule/schedule.db` — jobs and run history
  (`SCHEDULE_DB` overrides it, which is how the tool is tested without touching
  the real one)
