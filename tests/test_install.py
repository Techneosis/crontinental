"""The Claude Code half of install.sh, against a throwaway config directory.

Only --claude-only paths run here, so nothing touches systemd or ~/.local.

    python3 -m unittest discover tests
"""

import os
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
INSTALL = str(ROOT / "install.sh")
ORIGINAL = "# My rules\n\nSome existing rule.\n"


class ClaudeSetup(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        t = Path(self.tmp.name)
        (t / "claude").mkdir()
        (t / "dots").mkdir()
        # CLAUDE.md as a dotfiles-style symlink: the case that a rename would break
        self.real = t / "dots" / "CLAUDE.md"
        self.real.write_text(ORIGINAL)
        self.link = t / "claude" / "CLAUDE.md"
        self.link.symlink_to("../dots/CLAUDE.md")
        self.skill = t / "claude" / "skills" / "crontinental"
        self.env = {**os.environ, "CLAUDE_CONFIG_DIR": str(t / "claude")}

    def tearDown(self):
        self.tmp.cleanup()

    def run_(self, *args):
        p = subprocess.run([INSTALL, *args], env=self.env, capture_output=True, text=True,
                           stdin=subprocess.DEVNULL)
        self.assertEqual(p.returncode, 0, p.stderr)
        return p.stdout

    def test_without_a_terminal_it_asks_rather_than_writes(self):
        out = self.run_("--claude-only")
        self.assertIn("without asking", out)
        self.assertEqual(self.real.read_text(), ORIGINAL)
        self.assertTrue((self.skill / "SKILL.md").is_file(), "the skill is linked regardless")

    def test_section_is_added_once_and_through_the_symlink(self):
        self.run_("--claude-only", "--claude")
        self.run_("--claude-only", "--claude")
        text = self.real.read_text()
        self.assertTrue(text.startswith(ORIGINAL))
        self.assertEqual(text.count("crontinental:begin"), 1)
        self.assertIn((ROOT / "agents" / "claude-md-snippet.md").read_text(), text)
        self.assertTrue(self.link.is_symlink(), "CLAUDE.md must stay a symlink")

    def test_no_claude_does_nothing(self):
        self.run_("--claude-only", "--no-claude")
        self.assertFalse(self.skill.exists())
        self.assertEqual(self.real.read_text(), ORIGINAL)

    def test_uninstall_restores_the_file_exactly(self):
        self.run_("--claude-only", "--claude")
        self.run_("--uninstall", "--claude-only")
        self.assertEqual(self.real.read_text(), ORIGINAL)
        self.assertFalse(self.skill.exists())
        self.assertTrue(self.link.is_symlink())

    def test_missing_claude_code_is_skipped(self):
        self.env["CLAUDE_CONFIG_DIR"] = str(Path(self.tmp.name) / "nope")
        self.assertIn("not found", self.run_("--claude-only", "--claude"))


if __name__ == "__main__":
    unittest.main()
