"""Sessions whose end the watcher never saw: kills and failed exits."""

import pytest

from harness import activity_rows, pause, session_rows


def test_killed_watcher_session_is_recovered_on_next_start(fm, pg, watched):
    with fm.watch(watched) as w:
        f = watched / "a.txt"
        f.write_text("x")
        w.expect_event("CREATE", f)
        w.kill()  # nothing is saved: the 5-minute save never ran

    [s] = session_rows(pg, w.root)
    assert s["ended_at"] == "", "a killed watcher leaves its session open"

    with fm.watch(watched) as w2:
        w2.wait_for(r"Closed 1 session\(s\) left open")
        w2.end()

    [s] = session_rows(pg, w2.root)
    assert s["end_reason"] == "recovered"
    assert s["ended_at"] != ""


@pytest.mark.slow
def test_failed_exit_keeps_data_and_reclassifies_session(fm, pg, watched):
    doc = watched / "report.txt"
    doc.write_text("outline")  # exists before the session, so only saves happen
    with fm.watch(watched) as w:
        for i in range(5):
            doc.write_text(f"version {i}")
            w.expect_event("WRITE", doc)
            pause()
        pg.stop()  # the database goes away before the user types end
        try:
            w.end()
        finally:
            pg.start()
    w.wait_for(r"kept \d+ file entries in")
    assert list(fm.spill_dir.glob("unsaved-*.json")), "unsaved entries should be on disk"

    # Next start: the open session is closed as recovered, then the kept
    # entries are saved and the session is reclassified from them.
    with fm.watch(watched) as w2:
        w2.wait_for(r"recovered 1 unsaved file entries")
        w2.end()

    first = session_rows(pg, w2.root)[0]
    assert first["end_reason"] == "recovered"
    assert first["kind"] == "work"
    assert activity_rows(pg, doc), "recovered activity should be in the database"
    assert not list(fm.spill_dir.iterdir()), "spill folder should be empty once saved"
