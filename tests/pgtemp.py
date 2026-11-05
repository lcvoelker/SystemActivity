"""Throwaway PostgreSQL clusters for tests.

Each TempPostgres is a brand-new cluster: initdb into a temporary folder,
started on a free localhost port with trust authentication, with the
filemaster database created and db/bootstrap.sql applied. It never connects
to the real database and is deleted afterwards.

Library use (the pytest fixtures):

    pg = TempPostgres(); pg.start(); pg.create()
    pg.dsn("fm_app"); pg.query("SELECT ...")
    pg.destroy()

Command-line use (CI, or before `go test` locally):

    python tests/pgtemp.py start --dir <folder> --port 54329 [--github-env]
    python tests/pgtemp.py stop --dir <folder>

PostgreSQL's own programs (initdb, pg_ctl, psql) are found through
FILEMASTER_PG_BIN, PGBIN (set on GitHub's Windows runners), PATH, or a
standard "C:\\Program Files\\PostgreSQL\\<version>\\bin" install.
"""

from __future__ import annotations

import argparse
import os
import shutil
import socket
import subprocess
import sys
import tempfile
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
BOOTSTRAP = REPO / "db" / "bootstrap.sql"
DATABASE = "filemaster"
FIELD_SEP = "\x1f"  # unit separator: never appears in paths or values


def _exe(name: str) -> str:
    return name + (".exe" if os.name == "nt" else "")


def find_pg_bin() -> Path:
    candidates = [os.environ.get("FILEMASTER_PG_BIN"), os.environ.get("PGBIN")]
    on_path = shutil.which("initdb")
    if on_path:
        candidates.append(str(Path(on_path).parent))
    installs = Path(r"C:\Program Files\PostgreSQL")
    if installs.is_dir():
        versions = [p for p in installs.iterdir() if p.name.isdigit()]
        for v in sorted(versions, key=lambda p: int(p.name), reverse=True):
            candidates.append(str(v / "bin"))
    for c in candidates:
        if c and (Path(c) / _exe("initdb")).is_file():
            return Path(c)
    raise RuntimeError(
        "PostgreSQL programs not found; set FILEMASTER_PG_BIN to the folder containing initdb"
    )


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def sql_literal(value: str) -> str:
    """Quote a value as an SQL string literal."""
    return "'" + value.replace("'", "''") + "'"


class TempPostgres:
    def __init__(self, folder: str | Path | None = None, port: int | None = None):
        self.bin = find_pg_bin()
        self.folder = Path(folder) if folder else Path(tempfile.mkdtemp(prefix="filemaster-pg-"))
        self.data = self.folder / "data"
        self.log = self.folder / "pg.log"
        port_file = self.folder / "port"
        if port is None and port_file.is_file():
            port = int(port_file.read_text())
        self.port = port or free_port()

    def _run(self, program: str, *args: str, env: dict | None = None) -> str:
        result = subprocess.run(
            [str(self.bin / _exe(program)), *args],
            stdin=subprocess.DEVNULL,
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            env=env,
        )
        if result.returncode != 0:
            raise RuntimeError(f"{program} failed ({result.returncode}):\n{result.stdout}{result.stderr}")
        return result.stdout

    def _pg_ctl(self, *args: str, check: bool = True) -> None:
        # The server inherits pg_ctl's output handles. They must not be
        # pipes, or this call would wait for the server to exit.
        result = subprocess.run(
            [str(self.bin / _exe("pg_ctl")), "-D", str(self.data), *args],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        if check and result.returncode != 0:
            log = self.log.read_text(errors="replace") if self.log.exists() else ""
            raise RuntimeError(f"pg_ctl {' '.join(args)} failed ({result.returncode}):\n{log[-2000:]}")

    def start(self) -> None:
        """Create the cluster on first use, then start the server."""
        self.folder.mkdir(parents=True, exist_ok=True)
        if not self.data.exists():
            self._run("initdb", "-D", str(self.data), "-U", "postgres", "-A", "trust",
                      "-E", "UTF8", "--no-instructions")
            (self.folder / "port").write_text(str(self.port))
        self._pg_ctl("-o", f"-p {self.port} -c listen_addresses=localhost",
                     "-l", str(self.log), "-w", "-t", "60", "start")

    def stop(self) -> None:
        self._pg_ctl("-m", "fast", "-w", "stop", check=False)

    def create(self) -> None:
        """Create the filemaster database and apply db/bootstrap.sql (schema version 0)."""
        self._run("createdb", "-h", "localhost", "-p", str(self.port), "-U", "postgres", DATABASE)
        self._run("psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-h", "localhost",
                  "-p", str(self.port), "-U", "postgres", "-d", DATABASE, "-f", str(BOOTSTRAP))

    def destroy(self) -> None:
        self.stop()
        shutil.rmtree(self.folder, ignore_errors=True)

    def dsn(self, role: str = "fm_app") -> str:
        return f"postgres://{role}@localhost:{self.port}/{DATABASE}?sslmode=disable"

    def query(self, sql: str) -> list[list[str]]:
        """Run SQL as the superuser with search_path=filemaster.

        Returns rows of strings; NULL comes back as "".
        """
        env = {**os.environ, "PGOPTIONS": "-c search_path=filemaster", "PGCLIENTENCODING": "UTF8"}
        out = self._run("psql", "-X", "-A", "-t", "-q", "-v", "ON_ERROR_STOP=1",
                        "-F", FIELD_SEP, "-h", "localhost", "-p", str(self.port),
                        "-U", "postgres", "-d", DATABASE, "-c", sql, env=env)
        return [line.split(FIELD_SEP) for line in out.splitlines() if line]

    def scalar(self, sql: str) -> str:
        rows = self.query(sql)
        return rows[0][0] if rows else ""


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    sub = parser.add_subparsers(dest="command", required=True)
    start = sub.add_parser("start", help="create (if needed) and start a cluster")
    start.add_argument("--dir", required=True)
    start.add_argument("--port", type=int)
    start.add_argument("--github-env", action="store_true",
                       help="export the test database URLs to later GitHub Actions steps")
    stop = sub.add_parser("stop", help="stop a cluster and delete its folder")
    stop.add_argument("--dir", required=True)
    args = parser.parse_args()

    if args.command == "stop":
        TempPostgres(args.dir).destroy()
        return 0

    pg = TempPostgres(args.dir, args.port)
    fresh = not pg.data.exists()
    pg.start()
    if fresh:
        pg.create()
    urls = {
        "FILEMASTER_TEST_DATABASE_URL": pg.dsn("fm_app"),
        "FILEMASTER_TEST_MIGRATE_DATABASE_URL": pg.dsn("fm_owner"),
    }
    for name, url in urls.items():
        print(f"{name}={url}")
    if args.github_env:
        with open(os.environ["GITHUB_ENV"], "a", encoding="utf-8") as f:
            for name, url in urls.items():
                f.write(f"{name}={url}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
