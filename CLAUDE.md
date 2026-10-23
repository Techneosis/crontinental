# Working on Crontinental

This file is for working **on** Crontinental. The instructions for **using** it
(what an agent should do when asked to schedule something) are the skill in
`agents/skills/crontinental/SKILL.md`, which `install.sh` links into a user's
Claude Code setup.

## Layout

| path | what |
|---|---|
| `cmd/crontinental/` | the whole tool, in Go: one static binary (`CGO_ENABLED=0`) |
| `systemd/` | the tick (`crontinental.{timer,service}`) |
| `completions/` | bash completion |
| `agents/` | what ships to users' Claude Code: the skill, and the CLAUDE.md snippet |
| `install.sh` | links everything into place; `--claude-only` touches only Claude Code |
| `tests/` | `python3 -m unittest discover tests` — run before every commit (it builds the binary) |
| `docs/design.md` | the building plan: why each piece is the way it is |

## Rules

- **Run the tests.** They drive the real CLI against a throwaway ledger
  (`CRONTINENTAL_DB`) and a throwaway Claude config (`CLAUDE_CONFIG_DIR`), so
  they never touch the machine's own jobs. A behaviour change comes with a test
  that fails without it.
- **One static binary, no runtime.** Go's standard library plus
  `modernc.org/sqlite` (pure Go, so cross-compiling needs no C toolchain). A
  new dependency has to keep `CGO_ENABLED=0` builds working for every target.
- **The ledger schema is a contract.** Existing ledgers must keep working: a
  schema change bumps `schemaVersion` (`ledger.go`) and ships a migration that is tested
  against the previous version's schema (see `Migration` in the tests).
- **Keep the invariants in the tick:** advance a job's schedule *before*
  dispatching it (a failed launch must not retry every minute), and never reap a
  job that has a live run.
- **The README stays in the hotel frame.** The Ledger, the Concierge (tick), the
  Room Steward (`exec`), the Cleaners (the 3-day reap), the Rules. Every
  metaphor has to map to real behaviour; if the behaviour changes, so does the
  README. `docs/design.md` is the plain-language version.
- **`agents/` is for everyone.** No machine names, people or private paths in
  the skill or the snippet. Change the snippet only in
  `agents/claude-md-snippet.md`; installed copies are rewritten from it.
- **Commit messages carry no AI attribution**: no `Co-Authored-By`,
  `Claude-Session` or "Generated with" lines.

## The test suite is the spec

The tests are Python on purpose and black-box: they drive the binary through
its CLI (`CRONTINENTAL_BIN` picks which one; unset, they build it). That made
them the conformance suite when the tool was ported from Python to Go
(2026-10-23), and it keeps behaviour pinned to the CLI rather than to internals.

## Where it's going

macOS support (launchd for the tick, a process group per run, file logs) needs
the schedule grammars in Go first: today calendar specs and timespans go
through `systemd-analyze`, and times through GNU `date -d`. Then release
binaries per platform.
