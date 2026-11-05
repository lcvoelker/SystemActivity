"""Automatic migrations, the schema check, and confirmed rollbacks.

These use fresh_pg: a new cluster at schema version 0 for each test.
"""

import re
from concurrent.futures import ThreadPoolExecutor

from harness import Filemaster


def applied_versions(fm: Filemaster) -> list[int]:
    out = fm.pg.query("SELECT version_id FROM goose_db_version WHERE version_id > 0 ORDER BY 1")
    return [int(r[0]) for r in out]


def embedded_versions(fm: Filemaster) -> list[int]:
    status = fm.run("migrate", "status").stdout
    return sorted({int(v) for v in re.findall(r"-- (\d+)_\w+\.sql", status)})


def test_watch_migrates_a_fresh_database_by_itself(filemaster_exe, fresh_pg, tmp_path, watched):
    fm = Filemaster(filemaster_exe, fresh_pg, tmp_path)
    want = embedded_versions(fm)

    with fm.watch(watched) as w:
        for v in want:
            w.wait_for(rf"Applied database migration {v:05d}\.")
        w.end()

    assert applied_versions(fm) == want


def test_without_owner_url_it_refuses_to_start(filemaster_exe, fresh_pg, tmp_path):
    fm = Filemaster(filemaster_exe, fresh_pg, tmp_path)
    result = fm.run("sessions", migrate=False)
    assert result.returncode != 0
    assert "database schema is at version 0" in result.stdout
    assert applied_versions(fm) == []


def test_simultaneous_starts_apply_each_migration_once(filemaster_exe, fresh_pg, tmp_path):
    fm = Filemaster(filemaster_exe, fresh_pg, tmp_path)
    want = embedded_versions(fm)

    with ThreadPoolExecutor(2) as pool:
        results = list(pool.map(lambda _: fm.run("sessions"), range(2)))

    for r in results:
        assert r.returncode == 0, r.stdout
    applied = re.findall(r"Applied database migration (\d+)\.", results[0].stdout + results[1].stdout)
    assert sorted(int(v) for v in applied) == want, "each migration should be applied exactly once"


def test_rollback_needs_yes_and_lists_what_it_drops(filemaster_exe, fresh_pg, tmp_path):
    fm = Filemaster(filemaster_exe, fresh_pg, tmp_path)
    assert fm.run("migrate", "up").returncode == 0
    before = applied_versions(fm)

    refused = fm.run("migrate", "down")
    assert refused.returncode != 0
    assert f"{before[-1]:05d}" in refused.stdout
    assert "DROP" in refused.stdout
    assert "--yes" in refused.stdout
    assert applied_versions(fm) == before, "nothing may change without --yes"

    confirmed = fm.run("migrate", "down", "--yes")
    assert confirmed.returncode == 0, confirmed.stdout
    assert applied_versions(fm) == before[:-1]

    # And back up again, as auto-migration would.
    assert fm.run("migrate", "up").returncode == 0
    assert applied_versions(fm) == before
