"""Content history across save windows, and the 3-day retention."""

from harness import session_rows, sql_literal


def test_history_shows_each_content_version_and_duplicates(fm, pg, watched):
    a, b = watched / "a.txt", watched / "b.txt"

    # Each `end` saves, so two runs give two save windows.
    with fm.watch(watched) as w:
        a.write_text("first")
        w.expect_event("CREATE", a)
        w.end()
    with fm.watch(watched) as w:
        a.write_text("second")
        w.expect_event("WRITE", a)
        b.write_text("second")
        w.expect_event("CREATE", b)
        w.end()

    result = fm.run("history", str(a))
    assert result.returncode == 0, result.stdout
    versions = history_table(result.stdout)
    assert len(versions) == 2, result.stdout
    assert versions[0]["TO"] != "current" and versions[1]["TO"] == "current"
    assert versions[0]["SHA256"] != versions[1]["SHA256"]
    assert "Same content currently at:" in result.stdout
    assert str(b) in result.stdout


def history_table(output: str) -> list[dict[str, str]]:
    """Rows of the SHA256/SIZE/FROM/TO table printed by `filemaster history`."""
    lines = output.splitlines()
    start = next(i for i, l in enumerate(lines) if l.startswith("SHA256"))
    rows = []
    for line in lines[start + 1:]:
        if not line.strip():
            break
        sha, size, from_date, from_time, *to = line.split()
        rows.append({"SHA256": sha, "SIZE": size, "FROM": f"{from_date} {from_time}", "TO": " ".join(to)})
    return rows


def test_prune_deletes_data_older_than_three_days(fm, pg, watched):
    with fm.watch(watched) as w:
        f = watched / "recent.txt"
        f.write_text("keep me")
        w.expect_event("CREATE", f)
        w.end()
    [recent] = session_rows(pg, w.root)

    # An expired session, written directly as if recorded 5 days ago.
    old_root = str(watched) + "-old"
    pg.query(
        "WITH s AS (INSERT INTO sessions (host, root_path, started_at, ended_at, kind, end_reason) "
        f"VALUES ('test', {sql_literal(old_root)}, now() - interval '5 days', "
        "now() - interval '5 days', 'other', 'stopped') RETURNING id), "
        f"f AS (INSERT INTO files (path) VALUES ({sql_literal(old_root + chr(92) + 'old.txt')}) RETURNING id) "
        "SELECT ensure_file_activity_partition(now() - interval '5 days')"
    )
    pg.query(
        "INSERT INTO file_activity (session_id, file_id, first_at, last_at, creates, last_op) "
        "SELECT s.id, f.id, now() - interval '5 days', now() - interval '5 days', 1, 'CREATE' "
        f"FROM sessions s, files f WHERE s.root_path = {sql_literal(old_root)} "
        f"AND f.path = {sql_literal(old_root + chr(92) + 'old.txt')}"
    )

    result = fm.run("prune")
    assert result.returncode == 0, result.stdout

    assert session_rows(pg, old_root) == []
    assert pg.scalar(f"SELECT count(*) FROM files WHERE starts_with(path, {sql_literal(old_root)})") == "0"
    assert [s["id"] for s in session_rows(pg, w.root)] == [recent["id"]], "recent data must survive"
