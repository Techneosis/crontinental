"""The binary under test.

CRONTINENTAL_BIN points the suite at any build of the CLI: the tests are
black-box on purpose, so they are the conformance suite for any
implementation. Unset, the checkout is built once into a temp directory.
"""

import atexit
import os
import shutil
import subprocess
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent


def _build() -> str:
    out = tempfile.mkdtemp(prefix="crontinental-test-")
    atexit.register(shutil.rmtree, out, ignore_errors=True)
    path = os.path.join(out, "crontinental")
    subprocess.run(["go", "build", "-o", path, "./cmd/crontinental"], cwd=ROOT, check=True,
                   env={**os.environ, "CGO_ENABLED": "0"})
    return path


BIN = os.environ.get("CRONTINENTAL_BIN") or _build()
