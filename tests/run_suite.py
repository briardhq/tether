# SPDX-License-Identifier: GPL-3.0-or-later
#
# ⚠️ GPL-3 by association: it exists to run zigpy-znp's GPL-3 test suite. Nothing here is
# linked into the briard-tether binary or ships in any release artifact.
"""Run zigpy-znp's own application suite through tether.

`run_gate.py` proves one client boot. This runs the suite zigpy-znp uses to hold its ZHA-
facing behaviour, with tether in the middle of every one of them — ZDO requests, device joins,
callbacks, concurrent requests, timeouts, the things a byte pipe under load would break.

The mechanism is one file: their tests reach the radio only through the `make_znp_server`
fixture, so `application_conftest.py` is dropped in as `tests/application/conftest.py`, where
pytest's ordinary rule lets a nearer conftest override a farther one. Their tree is copied out
of the nix store to do it and their own files are never edited.

    go build -o briard-tether ./cmd/briard-tether
    python3 tests/run_suite.py                 # the application suite
    python3 tests/run_suite.py -- -k startup   # anything after -- goes to pytest

With `TETHER_WINDOWS_SSH` set, every one of those tests runs against a tether on a Windows
machine instead, the clients and the emulator staying here (see rig.py).

⚠️ This borrows somebody else's fixtures, which are a test suite's internals and not an API.
When it breaks after a zigpy-znp bump, the first question is whether they moved, not whether
tether did.
"""

from __future__ import annotations

import argparse
import os
import pathlib
import shutil
import stat
import subprocess
import sys
import tempfile

import rig

HERE = pathlib.Path(__file__).resolve().parent


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument(
        "--tether",
        type=pathlib.Path,
        default=pathlib.Path("./briard-tether"),
        help="the tether binary to put between the client and the emulator",
    )
    parser.add_argument(
        "--suite",
        default="application",
        help="which of their test directories to run (default: application, the ZHA-facing "
        "one; `api` tests their own driver internals and says little about a transport)",
    )
    parser.add_argument("pytest_args", nargs="*", help="passed to pytest after --")
    args = parser.parse_args()

    src = os.environ.get("ZIGPY_ZNP_SRC")
    if not src:
        print(
            "ZIGPY_ZNP_SRC is not set: run this inside `nix develop`. The suite lives in\n"
            "zigpy-znp's source tree, which no released package ships.",
            file=sys.stderr,
        )
        return 2

    tether = rig.binary(args.tether)

    with tempfile.TemporaryDirectory(prefix="tether-suite-") as tmp:
        root = pathlib.Path(tmp)
        # Copied rather than edited in place: the store is read-only, and their files should
        # come back byte-identical after a bump so the diff is only ever ours.
        shutil.copytree(pathlib.Path(src) / "tests", root / "tests")
        # The nix store is read-only and copytree preserves modes, so the copy arrives
        # unwritable and nothing can be dropped into it.
        for path in (root / "tests").rglob("*"):
            path.chmod(path.stat().st_mode | stat.S_IWUSR)
        target = root / "tests" / args.suite
        if not target.is_dir():
            print(f"no such suite: {args.suite}", file=sys.stderr)
            return 2
        shutil.copy(HERE / "application_conftest.py", target / "conftest.py")
        # Their pytest settings travel with their tests, and one of them is load-bearing:
        # asyncio_mode = "auto" is what makes their async tests run at all. Taking their file
        # rather than writing our own means their suite is configured the way they configure
        # it, which is the whole point of borrowing it.
        shutil.copy(pathlib.Path(src) / "pyproject.toml", root / "pyproject.toml")

        # HERE too, so the dropped-in conftest can import our rig alongside their tree.
        env = dict(os.environ, TETHER_BINARY=str(tether), PYTHONPATH=f"{root}:{HERE}")

        # Their `timeout = 20` (in the pyproject we take wholesale) is generous for a pty and
        # not for a machine boundary: measured, one of their tests takes ~0.5 s
        # against a local tether and ~12 s against one on Windows, so every test would fail on
        # the clock and none of them would be telling us anything. Raised only over the seam,
        # only when the caller did not say otherwise, and said out loud — a suite that is red
        # for a reason that has nothing to do with tether is worse than no suite.
        extra = []
        if rig.WINDOWS and not any(a.startswith("--timeout") for a in args.pytest_args):
            extra = ["--timeout=100"]
            print("their 20 s per-test timeout is raised to 100 s: the wire to the far machine "
                  "is slower than a pty", flush=True)
        print(f"running zigpy-znp's {args.suite} suite through {tether}\n", flush=True)
        return subprocess.run(
            [sys.executable, "-m", "pytest", str(target), "-p", "no:cacheprovider",
             *extra, *args.pytest_args],
            cwd=root,
            env=env,
        ).returncode


if __name__ == "__main__":
    sys.exit(main())
