# Working on Crontinental

This file is for working **on** Crontinental. The instructions for **using** it
(what an agent should do when asked to schedule something) are the skill in
`agents/skills/crontinental/SKILL.md`, which `install.sh` links into a user's
Claude Code setup.

## Layout

| path | what |
|---|---|
| `bin/crontinental` | the whole tool: one Python file, standard library only |
| `systemd/` | the tick (`crontinental.{timer,service}`) |
| `completions/` | bash completion |
| `agents/` | what ships to users' Claude Code: the skill, and the CLAUDE.md snippet |
| `install.sh` | links everything into place; `--claude-only` touches only Claude Code |
| `tests/` | `python3 -m unittest discover tests` — run before every commit |
| `docs/design.md` | the building plan: why each piece is the way it is |

## Rules

- **Run the tests.** They drive the real CLI against a throwaway ledger
  (`CRONTINENTAL_DB`) and a throwaway Claude config (`CLAUDE_CONFIG_DIR`), so
  they never touch the machine's own jobs. A behaviour change comes with a test
  that fails without it.
- **No dependencies outside the standard library.** The tool has to install by
  symlink, with nothing to pip-install.
- **The ledger schema is a contract.** Existing ledgers must keep working: a
  schema change bumps `SCHEMA_VERSION` and ships a migration that is tested
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

## Where it's going

A rewrite in Go is planned, for a single static binary per platform and macOS
support (launchd for the tick, a process group per run, file logs). Until it
lands, this Python version is the reference implementation: the test suite is
black-box on purpose, so it can be pointed at the Go binary and serve as the
conformance suite.
