"""End-to-end tests for crontinental, run against a throwaway ledger.

Nothing here touches systemd: runs are driven through `exec` directly (what a
job's own unit would call), and `tick` is only exercised on paths that finish or
reap jobs rather than dispatching them. So the suite runs anywhere Python and
`systemd-analyze` do.

    python3 -m unittest discover tests
"""

import os
import sqlite3
import subprocess
import tempfile
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path

BIN = str(Path(__file__).resolve().parent.parent / "bin" / "crontinental")

V1_SCHEMA = """
CREATE TABLE job (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, command TEXT NOT NULL,
  workdir TEXT, kind TEXT NOT NULL, spec TEXT, interval_sec INTEGER, next_run TEXT,
  catchup TEXT NOT NULL DEFAULT 'run', until TEXT, timeout_sec INTEGER NOT NULL,
  notify TEXT NOT NULL DEFAULT 'fail', enabled INTEGER NOT NULL DEFAULT 1,
  note TEXT, created_at TEXT NOT NULL, created_by TEXT);
CREATE TABLE run (
  id INTEGER PRIMARY KEY, job_id INTEGER NOT NULL REFERENCES job(id) ON DELETE CASCADE,
  unit TEXT, due_at TEXT, started_at TEXT, finished_at TEXT, status INTEGER,
  outcome TEXT NOT NULL, late_sec INTEGER, detail TEXT);
PRAGMA user_version=1;
"""


def iso(dt):
    return dt.replace(microsecond=0).isoformat()


def ago(**kw):
    return iso(datetime.now(timezone.utc) - timedelta(**kw))


class Ledger(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.db = os.path.join(self.tmp.name, "ledger.db")
        self.env = {**os.environ, "CRONTINENTAL_DB": self.db}

    def tearDown(self):
        self.tmp.cleanup()

    def cli(self, *args, ok=True):
        p = subprocess.run([BIN, *args], env=self.env, capture_output=True, text=True)
        if ok:
            self.assertEqual(p.returncode, 0, p.stderr)
        return p

    def sql(self, q, *params):
        with sqlite3.connect(self.db) as c:
            return c.execute(q, params).fetchall()

    def job(self, name):
        rows = self.sql("SELECT enabled, next_run, finished_at, forever, until_done"
                        " FROM job WHERE name=?", name)
        return rows[0] if rows else None


class Booking(Ledger):
    def test_repeat_without_an_end_is_refused(self):
        p = self.cli("every", "30m", "--name", "x", "--", "true", ok=False)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("has to say how it ends", p.stderr)

    def test_forever_contradicts_an_end(self):
        p = self.cli("every", "30m", "--name", "x", "--forever", "--until", "2 hours",
                     "--", "true", ok=False)
        self.assertIn("contradicts", p.stderr)

    def test_one_off_takes_no_end_flags(self):
        p = self.cli("in", "5m", "--name", "x", "--forever", "--", "true", ok=False)
        self.assertIn("for repeats", p.stderr)

    def test_until_in_the_past_is_refused(self):
        p = self.cli("every", "30m", "--name", "x", "--until", "1 hour ago",
                     "--", "true", ok=False)
        self.assertIn("in the past", p.stderr)

    def test_each_end_is_accepted_and_recorded(self):
        self.cli("every", "30m", "--name", "a", "--until", "2 hours", "--", "true")
        self.cli("every", "30m", "--name", "b", "--until-done", "--", "true")
        self.cli("every", "30m", "--name", "c", "--forever", "--", "true")
        self.assertEqual(self.job("b")[4], 1)
        self.assertEqual(self.job("c")[3], 1)

    def test_daily_with_a_time_explains_both_grammars(self):
        p = self.cli("every", "daily 03:00", "--name", "x", "--forever", "--", "true", ok=False)
        self.assertIn("as a calendar", p.stderr)
        self.assertIn("as an interval", p.stderr)


class Stays(Ledger):
    def exec_(self, name):
        return subprocess.run([BIN, "exec", name], env=self.env, capture_output=True, text=True)

    def outcomes(self, name):
        return self.sql("SELECT r.outcome, r.status FROM run r JOIN job j ON j.id=r.job_id"
                        " WHERE j.name=? ORDER BY r.id", name)

    def test_exit_99_finishes_the_job_as_a_success(self):
        self.cli("every", "1m", "--name", "poll", "--until-done", "--notify", "never",
                 "--", "exit 99")
        p = self.exec_("poll")
        self.assertEqual(p.returncode, 0, "done must not make the unit fail")
        self.assertEqual(self.outcomes("poll"), [("done", 99)])
        enabled, next_run, finished_at, *_ = self.job("poll")
        self.assertEqual((enabled, next_run), (0, None))
        self.assertIsNotNone(finished_at)
        self.assertNotIn("poll", self.cli("list").stdout)
        self.assertIn("done", self.cli("list", "--all").stdout)

    def test_ordinary_exits_leave_the_job_scheduled(self):
        self.cli("every", "1h", "--name", "ok", "--forever", "--", "true")
        self.cli("every", "1h", "--name", "bad", "--forever", "--notify", "never", "--", "exit 3")
        self.assertEqual(self.exec_("ok").returncode, 0)
        self.assertEqual(self.exec_("bad").returncode, 3)
        self.assertEqual(self.outcomes("ok"), [("ok", 0)])
        self.assertEqual(self.outcomes("bad"), [("fail", 3)])
        self.assertEqual(self.job("ok")[0], 1)
        self.assertEqual(self.job("bad")[0], 1)


class Checkout(Ledger):
    def test_deadline_finishes_a_due_job_without_running_it(self):
        self.cli("every", "30m", "--name", "w", "--until", "2 hours", "--", "true")
        self.sql("UPDATE job SET next_run=?, until=? WHERE name='w'", ago(minutes=5), ago(minutes=1))
        self.cli("tick")
        enabled, _, finished_at, *_ = self.job("w")
        self.assertEqual(enabled, 0)
        self.assertIsNotNone(finished_at)
        self.assertEqual(self.sql("SELECT count(*) FROM run"), [(0,)])

    def test_cleaners_come_three_days_after_checkout(self):
        self.cli("every", "1h", "--name", "old", "--forever", "--", "true")
        self.cli("every", "1h", "--name", "recent", "--forever", "--", "true")
        self.sql("UPDATE job SET enabled=0, next_run=NULL, finished_at=? WHERE name='old'", ago(days=4))
        self.sql("UPDATE job SET enabled=0, next_run=NULL, finished_at=? WHERE name='recent'", ago(days=1))
        self.cli("tick")
        self.assertIsNone(self.job("old"))
        self.assertIsNotNone(self.job("recent"))

    def test_a_guest_still_in_the_room_holds_off_the_cleaners(self):
        self.cli("every", "1h", "--name", "busy", "--forever", "--", "true")
        self.sql("UPDATE job SET enabled=0, next_run=NULL, finished_at=? WHERE name='busy'", ago(days=4))
        self.sql("INSERT INTO run (job_id, started_at, outcome)"
                 " SELECT id, ?, 'running' FROM job WHERE name='busy'", ago(hours=2))
        self.cli("tick")
        self.assertIsNotNone(self.job("busy"))
        # ...unless that "running" row is older than retention: an exec that died too hard to say so
        self.sql("UPDATE run SET started_at=?", ago(days=5))
        self.cli("tick")
        self.assertIsNone(self.job("busy"))

    def test_runs_go_with_their_job(self):
        self.cli("every", "1h", "--name", "gone", "--forever", "--", "true")
        subprocess.run([BIN, "exec", "gone"], env=self.env, capture_output=True)
        self.sql("UPDATE job SET enabled=0, next_run=NULL, finished_at=? WHERE name='gone'", ago(days=4))
        self.cli("tick")
        self.assertEqual(self.sql("SELECT count(*) FROM run"), [(0,)])

    def test_enable_takes_a_finished_repeat_off_the_clock(self):
        self.cli("every", "1h", "--name", "back", "--until-done", "--", "true")
        self.sql("UPDATE job SET enabled=0, next_run=NULL, finished_at=? WHERE name='back'", ago(days=1))
        self.cli("enable", "back")
        enabled, next_run, finished_at, *_ = self.job("back")
        self.assertEqual(enabled, 1)
        self.assertIsNotNone(next_run)
        self.assertIsNone(finished_at)

    def test_disabled_is_not_finished(self):
        self.cli("every", "1h", "--name", "held", "--forever", "--", "true")
        self.cli("disable", "held")
        self.sql("UPDATE job SET created_at=? WHERE name='held'", ago(days=30))
        self.cli("tick")
        self.cli("prune")
        self.assertIsNotNone(self.job("held"))


class Migration(Ledger):
    def test_v1_ledger_is_migrated_without_stopping_anything(self):
        with sqlite3.connect(self.db) as c:
            c.executescript(V1_SCHEMA)
            c.execute("INSERT INTO job (name, command, kind, spec, interval_sec, next_run,"
                      " timeout_sec, created_at) VALUES ('legacy','true','repeat','1h',3600,?,3600,?)",
                      (ago(minutes=-30), ago(days=10)))
            c.execute("INSERT INTO job (name, command, kind, spec, interval_sec, next_run, until,"
                      " timeout_sec, created_at) VALUES ('ending','true','repeat','1h',3600,?,?,3600,?)",
                      (ago(minutes=-30), ago(hours=-5), ago(days=10)))
            c.execute("INSERT INTO job (name, command, kind, next_run, enabled, timeout_sec, created_at)"
                      " VALUES ('fired','true','once',NULL,0,3600,?)", (ago(days=10),))
        self.cli("list")
        self.assertEqual(self.sql("PRAGMA user_version"), [(2,)])
        self.assertEqual(self.job("legacy")[3], 1, "open-ended repeats become --forever")
        self.assertEqual(self.job("ending")[3], 0, "a repeat with --until keeps its end")
        self.assertIsNotNone(self.job("fired")[2], "finished one-offs start the retention clock")
        self.cli("tick")
        self.assertIsNotNone(self.job("fired"), "...rather than being reaped on the first tick")


if __name__ == "__main__":
    unittest.main()
