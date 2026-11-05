"""What the watcher records, and what it must leave out."""

import hashlib
import os
import shutil
import time

from harness import activity_rows, pause, stored_paths


def test_only_files_changed_during_the_run_are_stored(fm, pg, watched):
    (watched / "untouched.txt").write_text("never changed")
    stale = watched / "stale.txt"
    stale.write_text("old")
    old = time.time() - 30 * 24 * 3600
    os.utime(stale, (old, old))

    with fm.watch(watched) as w:
        # A sync client or antivirus "touching" an old file: a WRITE event
        # whose modified time predates the watcher. Must be dropped.
        os.utime(stale, (old + 60, old + 60))
        report = watched / "report.txt"
        report.write_text("draft")
        w.expect_event("CREATE", report)
        assert w.end() == 0

    assert stored_paths(pg, w.root) == {"report.txt"}


def test_repeated_saves_merge_into_one_row(fm, pg, watched):
    doc = watched / "notes.txt"
    with fm.watch(watched) as w:
        for i in range(5):
            doc.write_text(f"save {i}")
            w.expect_event("WRITE", doc)
            pause()
        assert w.end() == 0

    rows = activity_rows(pg, doc)
    assert len(rows) == 1, "saves within one save window should merge into one row"
    # Windows often reports two writes per save (truncate, then write).
    assert int(rows[0]["writes"]) >= 5


def test_moved_in_folder_is_one_event_without_its_files(fm, pg, watched, tmp_path):
    outside = tmp_path / "outside" / "project"
    outside.mkdir(parents=True)
    (outside / "inner1.txt").write_text("x")
    (outside / "inner2.txt").write_text("y")

    with fm.watch(watched) as w:
        shutil.move(str(outside), str(watched / "project"))
        w.expect_event("CREATE", watched / "project")
        pause()
        assert w.end() == 0

    assert stored_paths(pg, w.root) == {"project"}


def test_rename_records_where_the_file_came_from(fm, pg, watched):
    draft, final = watched / "draft.txt", watched / "final.txt"
    with fm.watch(watched) as w:
        draft.write_text("text")
        w.expect_event("CREATE", draft)
        pause()
        draft.rename(final)
        w.expect_event("RENAME", final)
        assert w.end() == 0

    [row] = activity_rows(pg, final)
    assert row["old_path"] == str(draft)
    assert row["renames"] == "1"


def test_content_is_hashed_at_save_time(fm, pg, watched):
    doc = watched / "data.csv"
    with fm.watch(watched) as w:
        doc.write_bytes(b"a,b\n1,2\n")
        w.expect_event("CREATE", doc)
        assert w.end() == 0

    [row] = activity_rows(pg, doc)
    assert row["sha256"] == hashlib.sha256(b"a,b\n1,2\n").hexdigest()
    assert int(row["size_bytes"]) == 8
