package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lcvoelker/SystemActivity/internal/session"
)

func TestSpillRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 11, 4, 16, 0, 0, 0, time.UTC)
	in := []*activity{{
		sessionID: 7, path: `D:\look\a.txt`, oldPath: `D:\look\old.txt`,
		counts:  session.Counts{Creates: 1, Writes: 4, Renames: 1},
		firstAt: t0, lastAt: t0.Add(time.Minute), lastOp: "WRITE",
	}}
	if _, err := spill(dir, in); err != nil {
		t.Fatal(err)
	}

	got := claimSpilled(dir)
	if len(got) != 1 {
		t.Fatalf("claimed %d entries, want 1", len(got))
	}
	a := got[0]
	if a.sessionID != 7 || a.path != in[0].path || a.oldPath != in[0].oldPath ||
		a.counts != in[0].counts || !a.lastAt.Equal(in[0].lastAt) || a.lastOp != "WRITE" {
		t.Errorf("round trip changed the entry: %+v", a)
	}

	if again := claimSpilled(dir); len(again) != 0 {
		t.Errorf("second claim returned %d entries; files should be deleted once loaded", len(again))
	}
}

func TestSpillKeepsUnreadableFiles(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "unsaved-1-1.json")
	if err := os.WriteFile(bad, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := claimSpilled(dir); len(got) != 0 {
		t.Errorf("claimed %d entries from a corrupt file", len(got))
	}
	if _, err := os.Stat(bad + ".bad"); err != nil {
		t.Errorf("corrupt file not kept for inspection: %v", err)
	}
}

// A failed final save keeps the entries on disk; the next watcher saves them.
func TestUnsavedEntriesSurviveFailedFinalSave(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	root := t.TempDir()
	spillDir := t.TempDir()
	t.Cleanup(func() { cleanup(t, pool, root) })
	path := filepath.Join(root, "draft.txt")
	write(t, path, "draft")

	id, err := newStore(pool, root, "").StartSession(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// First watcher: its database connection is gone when it exits.
	deadPool, err := NewPool(ctx, os.Getenv("FILEMASTER_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	first := newStore(deadPool, root, spillDir)
	now := time.Now()
	first.Record(session.Event{SessionID: id, Path: path, Op: "CREATE", Timestamp: now})
	first.Record(session.Event{SessionID: id, Path: path, Op: "WRITE", Timestamp: now.Add(time.Second)})
	// An entry whose session was deleted must not block the others.
	first.Record(session.Event{SessionID: -1, Path: filepath.Join(root, "orphan.txt"), Op: "CREATE", Timestamp: now})
	deadPool.Close()
	first.Close()

	files, _ := filepath.Glob(filepath.Join(spillDir, "unsaved-*.json"))
	if len(files) != 1 {
		t.Fatalf("got %d spill files after failed final save, want 1", len(files))
	}

	// Next watcher recovers and saves them.
	newStore(pool, root, spillDir).Close()

	var creates, writes int
	if err := pool.QueryRow(ctx, `
		SELECT coalesce(sum(creates), 0), coalesce(sum(writes), 0)
		FROM file_activity WHERE session_id = $1`, id).Scan(&creates, &writes); err != nil {
		t.Fatal(err)
	}
	if creates != 1 || writes != 1 {
		t.Errorf("recovered activity: %d creates, %d writes; want 1 and 1", creates, writes)
	}
	if left, _ := filepath.Glob(filepath.Join(spillDir, "*")); len(left) != 0 {
		t.Errorf("spill directory not emptied: %v", left)
	}
}

// The database is down when the user types end, so both the final save and
// closing the session fail. The next start closes the session as an orphan
// with nothing saved ("other", 0s); once its spilled activity is saved, the
// session must be reclassified from the real counts and its end time moved
// to the last activity.
func TestRecoveredSessionIsReclassified(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	root := t.TempDir()
	spillDir := t.TempDir()
	t.Cleanup(func() { cleanup(t, pool, root) })
	path := filepath.Join(root, "report.txt")
	write(t, path, "final text")

	start := time.Now().UTC().Truncate(time.Millisecond)
	id, err := newStore(pool, root, "").StartSession(ctx, start)
	if err != nil {
		t.Fatal(err)
	}

	deadPool, err := NewPool(ctx, os.Getenv("FILEMASTER_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	first := newStore(deadPool, root, spillDir)
	first.Record(session.Event{SessionID: id, Path: path, Op: "WRITE", Timestamp: start})
	for i := 1; i <= 4; i++ {
		first.Record(session.Event{SessionID: id, Path: path, Op: "WRITE", Timestamp: start.Add(time.Duration(i) * time.Second)})
	}
	deadPool.Close()
	first.Close()

	// Next start: orphan recovery runs before the spilled data is saved.
	if _, err := newStore(pool, root, "").CloseOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	assertSession(t, pool, id, "other", "recovered", start)

	// The recovered activity is saved: kind and end time are corrected.
	newStore(pool, root, spillDir).Close()
	assertSession(t, pool, id, "work", "recovered", start.Add(4*time.Second))
}

func assertSession(t *testing.T, pool *pgxpool.Pool, id int64, wantKind, wantReason string, wantEnd time.Time) {
	t.Helper()
	var kind, reason string
	var endedAt time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT kind, end_reason, ended_at FROM sessions WHERE id = $1`, id).Scan(&kind, &reason, &endedAt); err != nil {
		t.Fatal(err)
	}
	if kind != wantKind || reason != wantReason || !endedAt.Equal(wantEnd) {
		t.Errorf("session %d = %s/%s ending %v; want %s/%s ending %v",
			id, kind, reason, endedAt, wantKind, wantReason, wantEnd)
	}
}
