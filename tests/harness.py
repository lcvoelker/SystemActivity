"""Drives the real filemaster program for end-to-end tests.

Every run is isolated from the developer's machine:
- DATABASE_URL / MIGRATE_DATABASE_URL point at a throwaway cluster (pgtemp);
- the working directory is a temp folder, so the repo's .env is never loaded;
- LOCALAPPDATA points at a temp folder, so the real spill directory
  (%LOCALAPPDATA%\\filemaster\\unsaved) is never read or written.
"""

from __future__ import annotations

import os
import queue
import re
import subprocess
import threading
import time
from pathlib import Path

from pgtemp import TempPostgres, sql_literal

# Variables from the developer's environment that must never reach a test run.
_SCRUB = ("DATABASE_URL", "MIGRATE_DATABASE_URL", "FILEMASTER_TEST_DATABASE_URL",
          "FILEMASTER_TEST_MIGRATE_DATABASE_URL")

DEFAULT_TIMEOUT = 20.0


class Filemaster:
    """The filemaster CLI bound to one throwaway database and temp folders."""

    def __init__(self, exe: Path, pg: TempPostgres, workdir: Path):
        self.exe = exe
        self.pg = pg
        self.cwd = workdir / "cwd"
        self.appdata = workdir / "appdata"
        self.cwd.mkdir(parents=True, exist_ok=True)
        self.appdata.mkdir(parents=True, exist_ok=True)

    @property
    def spill_dir(self) -> Path:
        return self.appdata / "filemaster" / "unsaved"

    def env(self, migrate: bool = True) -> dict[str, str]:
        env = {k: v for k, v in os.environ.items() if k not in _SCRUB}
        env["DATABASE_URL"] = self.pg.dsn("fm_app")
        if migrate:
            env["MIGRATE_DATABASE_URL"] = self.pg.dsn("fm_owner")
        env["LOCALAPPDATA"] = str(self.appdata)
        return env

    def run(self, *args: str, migrate: bool = True, timeout: float = 60) -> subprocess.CompletedProcess:
        """Run a command to completion; output is stdout and stderr combined."""
        return subprocess.run(
            [str(self.exe), *args],
            cwd=self.cwd,
            env=self.env(migrate),
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=timeout,
        )

    def watch(self, root: Path, *flags: str, migrate: bool = True) -> "Watcher":
        return Watcher(self, root, flags, migrate)


class Watcher:
    """A running `filemaster watch`, with its output collected line by line."""

    def __init__(self, fm: Filemaster, root: Path, flags: tuple[str, ...], migrate: bool):
        self.lines: list[str] = []
        self._new: queue.Queue[str] = queue.Queue()
        self.proc = subprocess.Popen(
            [str(fm.exe), "watch", *flags, str(root)],
            cwd=fm.cwd,
            env=fm.env(migrate),
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            encoding="utf-8",
            errors="replace",
            bufsize=1,
        )
        threading.Thread(target=self._read, daemon=True).start()
        self.root = self.wait_for(r"^Watching:\s+(.+)$").group(1).strip()
        self.wait_for(r"Watcher started")

    def _read(self) -> None:
        for line in self.proc.stdout:
            line = line.rstrip("\r\n")
            self.lines.append(line)
            self._new.put(line)

    @property
    def output(self) -> str:
        return "\n".join(self.lines)

    def wait_for(self, pattern: str, timeout: float = DEFAULT_TIMEOUT) -> re.Match:
        """Wait until a line matches pattern (a regex); fail with the output so far."""
        rx = re.compile(pattern)
        for line in self.lines:
            if m := rx.search(line):
                return m
        deadline = time.monotonic() + timeout
        while (left := deadline - time.monotonic()) > 0:
            try:
                line = self._new.get(timeout=left)
            except queue.Empty:
                break
            if m := rx.search(line):
                return m
            if self.proc.poll() is not None and self._new.empty():
                break
        raise AssertionError(f"no line matching {pattern!r} within {timeout}s; output:\n{self.output}")

    def expect_event(self, op: str, path: Path | str, timeout: float = DEFAULT_TIMEOUT) -> None:
        self.wait_for(rf"^EVENT: {op} {re.escape(str(path))}$", timeout)

    def end(self, timeout: float = 60) -> int:
        """Type "end" like the user would and wait for the program to exit."""
        self.proc.stdin.write("end\n")
        self.proc.stdin.flush()
        code = self.proc.wait(timeout)
        time.sleep(0.1)  # let the reader thread drain the last lines
        return code

    def kill(self) -> None:
        """Terminate the process outright, as if killed from Task Manager."""
        self.proc.kill()
        self.proc.wait(10)

    def __enter__(self) -> "Watcher":
        return self

    def __exit__(self, *exc) -> None:
        if self.proc.poll() is None:
            self.kill()


def pause(seconds: float = 0.3) -> None:
    """Space out file operations so each produces its own events."""
    time.sleep(seconds)


def session_rows(pg: TempPostgres, root: str) -> list[dict[str, str]]:
    """Sessions for one watched root, oldest first."""
    cols = ["id", "kind", "end_reason", "started_at", "ended_at", "total_events",
            "creates", "writes", "renames", "removes"]
    rows = pg.query(
        f"SELECT session_id, kind, end_reason, started_at, ended_at, total_events, "
        f"creates, writes, renames, removes FROM session_summary "
        f"WHERE root_path = {sql_literal(root)} ORDER BY session_id"
    )
    return [dict(zip(cols, r)) for r in rows]


def activity_rows(pg: TempPostgres, path: Path | str) -> list[dict[str, str]]:
    """file_activity rows for one path, oldest first."""
    cols = ["session_id", "creates", "writes", "renames", "removes", "last_op",
            "old_path", "size_bytes", "sha256"]
    rows = pg.query(
        "SELECT a.session_id, a.creates, a.writes, a.renames, a.removes, a.last_op, "
        "a.old_path, a.size_bytes, encode(a.sha256, 'hex') "
        "FROM file_activity a JOIN files f ON f.id = a.file_id "
        f"WHERE f.path = {sql_literal(str(path))} ORDER BY a.last_at"
    )
    return [dict(zip(cols, r)) for r in rows]


def stored_paths(pg: TempPostgres, root: str) -> set[str]:
    """Names (relative to root) of every path stored for this root."""
    prefix = root.rstrip("\\/") + os.sep
    rows = pg.query(f"SELECT path FROM files WHERE starts_with(path, {sql_literal(prefix)})")
    return {r[0][len(prefix):] for r in rows}
