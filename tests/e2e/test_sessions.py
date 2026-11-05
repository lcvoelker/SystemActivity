"""How sessions are split, classified, labeled and summarized.

Rules (internal/session/classify.go, behavior.go): writes are work;
creates, renames, moves and deletes are organization, except that a file
created during the session and then saved counts as work, including its
renames. A behavior is in the label if it is at least 20% of the session's
events; both → "work + organization" (stored as "mixed"); fewer than 3
events → "brief".
"""

import re

import pytest

from harness import pause, session_rows


def test_frequent_saves_are_a_work_session(fm, pg, watched):
    doc = watched / "essay.txt"
    doc.write_text("outline")  # exists before the session, so only saves happen
    with fm.watch(watched) as w:
        for i in range(6):
            doc.write_text("paragraph " * (i + 1))
            w.expect_event("WRITE", doc)
            pause()
        w.end()

    [s] = session_rows(pg, w.root)
    assert s["kind"] == "work"
    assert s["end_reason"] == "stopped"


def test_new_document_saved_and_renamed_is_work(fm, pg, watched):
    # Creating a file and saving it is working on a new document; renaming
    # that new file (draft -> final) is still part of the work.
    draft, final = watched / "draft.docx", watched / "final.docx"
    with fm.watch(watched) as w:
        draft.write_text("first words")
        w.expect_event("CREATE", draft)
        pause()
        for i in range(3):
            draft.write_text("more words " * (i + 2))
            w.expect_event("WRITE", draft)
            pause()
        draft.rename(final)
        w.expect_event("RENAME", final)
        w.end()

    [s] = session_rows(pg, w.root)
    assert s["kind"] == "work"
    assert "session type: work" in w.lines


def test_reorganizing_is_an_organization_session(fm, pg, watched):
    names = [f"scan{i}.pdf" for i in range(4)]
    for n in names:
        (watched / n).write_text("pdf")

    with fm.watch(watched) as w:
        archive = watched / "archive"
        archive.mkdir()
        w.expect_event("CREATE", archive)
        for n in names[:3]:
            (watched / n).rename(watched / f"2026-{n}")
            w.expect_event("RENAME", watched / f"2026-{n}")
            pause()
        (watched / names[3]).unlink()
        w.expect_event("REMOVE", watched / names[3])
        w.end()

    [s] = session_rows(pg, w.root)
    assert s["kind"] == "organization"


def test_saving_and_tidying_is_work_plus_organization(fm, pg, watched):
    # The session from 2026-11-05 that used to come out as "other".
    for name in ["essayDraft.txt", "test1.pdf", "proposal.txt", "pass2.pdf", "resume1.pdf", "resume7.pdf"]:
        (watched / name).write_text("before")

    with fm.watch(watched) as w:
        (watched / "essayDraft.txt").rename(watched / "essayFinal.txt")
        w.expect_event("RENAME", watched / "essayFinal.txt")
        pause()
        (watched / "test1.pdf").rename(watched / "TEST.pdf")
        w.expect_event("RENAME", watched / "TEST.pdf")
        pause()
        (watched / "proposal.txt").unlink()
        w.expect_event("REMOVE", watched / "proposal.txt")
        for name in ["pass2.pdf", "resume1.pdf", "resume7.pdf"]:
            (watched / name).write_text("after")
            w.expect_event("WRITE", watched / name)
            pause()
        w.end()

    [s] = session_rows(pg, w.root)
    assert s["kind"] == "mixed"
    summary = w.output
    assert "session type: work + organization" in summary
    assert re.search(r"^WRITE: \d+ \(pass2\.pdf; resume1\.pdf; resume7\.pdf\)$", summary, re.M), summary
    assert "RENAME: 2 (essayDraft.txt -> essayFinal.txt; test1.pdf -> TEST.pdf)" in summary
    assert "REMOVE: 1 (proposal.txt)" in summary


def test_a_few_events_are_brief(fm, pg, watched):
    with fm.watch(watched) as w:
        f = watched / "one.txt"
        f.write_text("x")
        w.expect_event("CREATE", f)
        w.end()

    [s] = session_rows(pg, w.root)
    assert s["kind"] == "brief"


@pytest.mark.slow
def test_inactivity_splits_sessions(fm, pg, watched):
    with fm.watch(watched, "-idle", "2s") as w:
        first = watched / "first.txt"
        first.write_text("1")
        w.expect_event("CREATE", first)
        pause(3.0)  # longer than -idle
        second = watched / "second.txt"
        second.write_text("2")
        w.expect_event("CREATE", second)
        w.wait_for(r"Session \d+ closed \(idle\)")
        w.end()

    sessions = session_rows(pg, w.root)
    assert [s["end_reason"] for s in sessions] == ["idle", "stopped"]


def test_sessions_command_shows_kind_and_end(fm, pg, watched):
    with fm.watch(watched) as w:
        f = watched / "a.txt"
        f.write_text("x")
        w.expect_event("CREATE", f)
        w.end()

    result = fm.run("sessions", "-n", "5")
    assert result.returncode == 0, result.stdout
    header = result.stdout.splitlines()[0]
    assert "KIND" in header and "ENDED BY" in header
    assert "stopped" in result.stdout


def test_session_command_prints_saved_summary_with_files(fm, pg, watched):
    (watched / "draft.txt").write_text("v1")
    with fm.watch(watched) as w:
        (watched / "draft.txt").rename(watched / "final.txt")
        w.expect_event("RENAME", watched / "final.txt")
        pause()
        (watched / "notes.txt").write_text("new")
        w.expect_event("CREATE", watched / "notes.txt")
        w.end()

    [s] = session_rows(pg, w.root)
    result = fm.run("session", s["id"])
    assert result.returncode == 0, result.stdout
    assert f"=== Session {s['id']} summary ===" in result.stdout
    assert "ended by: stopped" in result.stdout
    assert "RENAME: 1 (draft.txt -> final.txt)" in result.stdout
    assert re.search(r"^CREATE: \d+ \(notes\.txt\)$", result.stdout, re.M), result.stdout

    missing = fm.run("session", "999999")
    assert missing.returncode != 0
    assert "no session 999999" in missing.stdout
