package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lcvoelker/SystemActivity/internal/db"
	"github.com/lcvoelker/SystemActivity/internal/session"
)

// Integration tests against a migrated database, connecting as fm_app.
// They read FILEMASTER_TEST_DATABASE_URL (never DATABASE_URL or .env) so
// they can't run against real data by accident: TestPruneExpired deletes
// everything older than the retention period.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("FILEMASTER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("FILEMASTER_TEST_DATABASE_URL not set")
	}
	pool, err := NewPool(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close) // registered first so it runs after row cleanup
	return pool
}

// recordAndSave records events through a fresh Store and closes it, which
// saves them immediately instead of waiting for the 5-minute save.
func recordAndSave(pool *pgxpool.Pool, root string, events ...session.Event) {
	s := NewStore(pool, root)
	for _, e := range events {
		s.Record(e)
	}
	s.Close()
}

func TestStoreMergesActivityAndTracksVersions(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	root := t.TempDir()
	t.Cleanup(func() { cleanup(t, pool, root) })
	a, b := filepath.Join(root, "a.txt"), filepath.Join(root, "b.txt")

	id, err := NewStore(pool, root).StartSession(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	ev := func(path, op string, s int) session.Event {
		return session.Event{SessionID: id, Path: path, Op: op, Timestamp: at(s)}
	}

	// Window 1: a file created and saved twice merges into one row.
	write(t, a, "version A")
	recordAndSave(pool, root, ev(a, "CREATE", 0), ev(a, "WRITE", 1), ev(a, "WRITE", 2))

	// Window 2: a changes; b is created with the same content.
	write(t, a, "version B")
	write(t, b, "version B")
	recordAndSave(pool, root, ev(a, "WRITE", 3), ev(b, "CREATE", 4))

	// Window 3: a is removed.
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	recordAndSave(pool, root, ev(a, "REMOVE", 5))

	var rows, creates, writes int
	if err := pool.QueryRow(ctx, `
		SELECT count(*), sum(creates), sum(writes)
		FROM file_activity JOIN files f ON f.id = file_id
		WHERE f.path = $1`, a).Scan(&rows, &creates, &writes); err != nil {
		t.Fatal(err)
	}
	if rows != 3 || creates != 1 || writes != 3 {
		t.Errorf("a.txt activity: %d rows, %d creates, %d writes; want 3, 1, 3", rows, creates, writes)
	}

	q := db.New(pool)
	history, err := q.FileHistory(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("got %d versions of a.txt, want 2: %+v", len(history), history)
	}
	if !bytes.Equal(history[0].Sha256, sum("version A")) || history[0].ValidTo == nil || !history[0].ValidTo.Equal(at(3)) {
		t.Errorf("first version = %x until %v, want A until %v", history[0].Sha256, history[0].ValidTo, at(3))
	}
	if !bytes.Equal(history[1].Sha256, sum("version B")) || history[1].ValidTo == nil || !history[1].ValidTo.Equal(at(5)) {
		t.Errorf("second version = %x until %v, want B closed at %v by REMOVE", history[1].Sha256, history[1].ValidTo, at(5))
	}

	dupes, err := q.PathsWithContent(ctx, db.PathsWithContentParams{Sha256: sum("version B"), ExcludePath: a})
	if err != nil {
		t.Fatal(err)
	}
	if len(dupes) != 1 || dupes[0] != b {
		t.Errorf("paths currently holding B = %v, want [%s]", dupes, b)
	}

	if err := NewStore(pool, root).EndSession(ctx, id, at(5), session.KindWork); err != nil {
		t.Fatal(err)
	}
	var total int64
	var kind string
	if err := pool.QueryRow(ctx, `SELECT total_events, kind FROM session_summary WHERE session_id = $1`, id).
		Scan(&total, &kind); err != nil {
		t.Fatal(err)
	}
	if total != 6 || kind != "work" {
		t.Errorf("session_summary = %d events, kind %q; want 6, work", total, kind)
	}
}

func TestCloseOrphans(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	root := t.TempDir()
	t.Cleanup(func() { cleanup(t, pool, root) })

	// A watcher that saved some reorganizing, then was killed before EndSession.
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	id, err := NewStore(pool, root).StartSession(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	var events []session.Event
	for i, name := range []string{"x", "y", "z"} {
		events = append(events, session.Event{
			SessionID: id, Op: "RENAME", Timestamp: t0.Add(time.Duration(i) * time.Second),
			Path: filepath.Join(root, name+"-new"), OldPath: filepath.Join(root, name),
		})
	}
	recordAndSave(pool, root, events...)

	n, err := NewStore(pool, root).CloseOrphans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("closed %d orphaned sessions, want 1", n)
	}
	var endedAt time.Time
	var kind string
	if err := pool.QueryRow(ctx, `SELECT ended_at, kind FROM sessions WHERE id = $1`, id).Scan(&endedAt, &kind); err != nil {
		t.Fatal(err)
	}
	if want := t0.Add(2 * time.Second); !endedAt.Equal(want) || kind != "organization" {
		t.Errorf("orphan closed at %v as %q, want %v as organization", endedAt, kind, want)
	}
}

func TestPruneExpired(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	root := t.TempDir()
	t.Cleanup(func() { cleanup(t, pool, root) })
	path := filepath.Join(root, "old.txt")
	write(t, path, "old content")

	old := time.Now().Add(-retention - 48*time.Hour)
	store := NewStore(pool, root)
	id, err := store.StartSession(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	recordAndSave(pool, root, session.Event{SessionID: id, Path: path, Op: "CREATE", Timestamp: old})
	if err := store.EndSession(ctx, id, old, session.KindOther); err != nil {
		t.Fatal(err)
	}

	res, err := PruneExpired(ctx, db.New(pool))
	if err != nil {
		t.Fatal(err)
	}
	if res.DeletedSessions < 1 || res.DroppedPartitions < 1 {
		t.Errorf("prune = %+v, want at least 1 session and 1 partition", res)
	}
	var left int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM sessions WHERE id = $1)
		     + (SELECT count(*) FROM files WHERE path = $2)`, id, path).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d rows of the expired session remain", left)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sum(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func cleanup(t *testing.T, pool *pgxpool.Pool, root string) {
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM file_versions WHERE file_id IN (SELECT id FROM files WHERE starts_with(path, $1 || '\'))`,
		`DELETE FROM file_activity WHERE file_id IN (SELECT id FROM files WHERE starts_with(path, $1 || '\'))`,
		`DELETE FROM files WHERE starts_with(path, $1 || '\')`,
		`DELETE FROM sessions WHERE root_path = $1`,
	} {
		if _, err := pool.Exec(ctx, stmt, root); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}
}
