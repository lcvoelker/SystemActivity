"""Fixtures for the end-to-end suite.

    pg        one throwaway cluster for the whole run, migrated to the latest version
    fresh_pg  a new cluster per test, bootstrapped but never migrated (version 0)
    fm        the filemaster CLI bound to `pg`, with its own temp cwd and LOCALAPPDATA
    watched   an empty folder to watch

The binary under test is FILEMASTER_BIN if set (CI builds it with -cover),
otherwise it is built here with `go build`.
"""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

import pytest

from harness import Filemaster
from pgtemp import REPO, TempPostgres

SMART_APP_CONTROL_HELP = (
    "Windows blocked the freshly built filemaster.exe (Smart App Control blocks new, "
    "unsigned programs). Nothing is wrong with the tests: run them on GitHub Actions "
    "(Actions tab > tests > Run workflow) instead of turning protection off."
)


@pytest.fixture(scope="session")
def filemaster_exe(tmp_path_factory) -> Path:
    exe = os.environ.get("FILEMASTER_BIN")
    if exe:
        path = Path(exe)
    else:
        path = tmp_path_factory.mktemp("bin") / ("filemaster.exe" if os.name == "nt" else "filemaster")
        build = ["go", "build", "-o", str(path)]
        if os.environ.get("GOCOVERDIR"):
            build.append("-cover")
        subprocess.run([*build, "./cmd/filemaster"], cwd=REPO, check=True)

    if cover := os.environ.get("GOCOVERDIR"):
        Path(cover).mkdir(parents=True, exist_ok=True)

    # Running it with no arguments prints usage and exits 2. If Windows
    # refuses to start it, stop the whole run with an explanation.
    try:
        subprocess.run([str(path)], capture_output=True, timeout=30)
    except OSError as e:
        pytest.exit(f"{SMART_APP_CONTROL_HELP}\n({e})", returncode=3)
    return path


@pytest.fixture(scope="session")
def pg(tmp_path_factory, filemaster_exe):
    cluster = TempPostgres(tmp_path_factory.mktemp("pg"))
    cluster.start()
    cluster.create()
    try:
        fm = Filemaster(filemaster_exe, cluster, tmp_path_factory.mktemp("migrate"))
        result = fm.run("migrate", "up")
        assert result.returncode == 0, result.stdout
        yield cluster
    finally:
        cluster.destroy()


@pytest.fixture
def fresh_pg(tmp_path):
    cluster = TempPostgres(tmp_path / "pg")
    cluster.start()
    cluster.create()
    try:
        yield cluster
    finally:
        cluster.destroy()


@pytest.fixture
def fm(filemaster_exe, pg, tmp_path) -> Filemaster:
    return Filemaster(filemaster_exe, pg, tmp_path)


@pytest.fixture
def watched(tmp_path) -> Path:
    folder = tmp_path / "look"
    folder.mkdir()
    return folder
