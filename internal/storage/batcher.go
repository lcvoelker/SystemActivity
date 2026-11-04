package storage

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lcvoelker/SystemActivity/internal/db"
	"github.com/lcvoelker/SystemActivity/internal/session"
)

// Agreed with the project owner; ask before changing (see AGENTS.md).
const (
	// How often buffered activity is saved. 5 minutes for testing.
	saveInterval = 5 * time.Minute
	// Most distinct files held in memory. Reaching it saves early; while
	// saves are failing, the oldest entries are dropped past it instead.
	maxEntries = 1000
	// After a failed save, data is retried at the next scheduled save.

	saveTimeout = 30 * time.Second
)

// activity is everything that happened to one path in one session during
// the current save window. Repeated saves to a file become one entry with
// a write count, so memory grows with files touched, not events.
type activity struct {
	sessionID int64
	path      string
	oldPath   string // latest path this one was renamed or moved from
	counts    session.Counts
	firstAt   time.Time
	lastAt    time.Time
	lastOp    string
}

type activityKey struct {
	sessionID int64
	path      string
}

type saveResult struct {
	batch []*activity
	err   error
}

// batcher merges events per file and saves them every saveInterval, in one
// transaction per save: upsert paths and blobs, COPY the activity rows, then
// pipeline the version updates. Saves run in their own goroutine so hashing
// and database time never block event intake.
type batcher struct {
	pool    *pgxpool.Pool
	q       *db.Queries
	in      chan session.Event
	results chan saveResult
	done    chan struct{}

	// Owned by run.
	pending    map[activityKey]*activity
	saving     bool
	lastFailed bool
	dropped    int

	// Owned by whichever goroutine is saving (one at a time).
	partitions map[string]bool // UTC days ("2006-01-02") known to have a partition
}

func newBatcher(pool *pgxpool.Pool, q *db.Queries) *batcher {
	b := &batcher{
		pool:       pool,
		q:          q,
		in:         make(chan session.Event, 256),
		results:    make(chan saveResult),
		done:       make(chan struct{}),
		pending:    make(map[activityKey]*activity),
		partitions: make(map[string]bool),
	}
	go b.run()
	return b
}

func (b *batcher) add(e session.Event) {
	b.in <- e
}

// close stops accepting events, waits for any save in progress and saves
// what's left.
func (b *batcher) close() {
	close(b.in)
	<-b.done
}

func (b *batcher) run() {
	defer close(b.done)
	ticker := time.NewTicker(saveInterval)
	defer ticker.Stop()

	for {
		select {
		case e, ok := <-b.in:
			if !ok {
				b.shutdown()
				return
			}
			b.merge(fromEvent(e))
			if len(b.pending) >= maxEntries {
				if !b.saving && !b.lastFailed {
					b.startSave()
				} else {
					b.trim()
				}
			}
		case <-ticker.C:
			if !b.saving {
				b.startSave()
			}
		case r := <-b.results:
			b.finishSave(r)
		}
	}
}

func (b *batcher) startSave() {
	if len(b.pending) == 0 {
		return
	}
	batch := make([]*activity, 0, len(b.pending))
	for _, a := range b.pending {
		batch = append(batch, a)
	}
	b.pending = make(map[activityKey]*activity)
	b.saving = true
	go func() { b.results <- saveResult{batch: batch, err: b.save(batch)} }()
}

func (b *batcher) finishSave(r saveResult) {
	b.saving = false
	if r.err != nil {
		log.Printf("storage: saving %d file entries failed, retrying at next save: %v", len(r.batch), r.err)
		b.lastFailed = true
		for _, a := range r.batch {
			b.merge(a)
		}
		b.trim()
		return
	}
	b.lastFailed = false
	if b.dropped > 0 {
		log.Printf("storage: %d file entries were dropped while saves were failing", b.dropped)
		b.dropped = 0
	}
}

func (b *batcher) shutdown() {
	if b.saving {
		b.finishSave(<-b.results)
	}
	b.startSave()
	if b.saving {
		r := <-b.results
		b.saving = false
		if r.err != nil {
			log.Printf("storage: %d file entries were not saved: %v", len(r.batch), r.err)
		}
	}
}

// merge folds a into the pending entry for the same session and path.
func (b *batcher) merge(a *activity) {
	k := activityKey{a.sessionID, a.path}
	cur, ok := b.pending[k]
	if !ok {
		b.pending[k] = a
		return
	}
	cur.counts.Creates += a.counts.Creates
	cur.counts.Writes += a.counts.Writes
	cur.counts.Renames += a.counts.Renames
	cur.counts.Removes += a.counts.Removes
	if a.firstAt.Before(cur.firstAt) {
		cur.firstAt = a.firstAt
	}
	if !a.lastAt.Before(cur.lastAt) {
		cur.lastAt = a.lastAt
		cur.lastOp = a.lastOp
		if a.oldPath != "" {
			cur.oldPath = a.oldPath
		}
	} else if cur.oldPath == "" {
		cur.oldPath = a.oldPath
	}
}

// trim drops the least recently active entries beyond maxEntries.
func (b *batcher) trim() {
	for len(b.pending) > maxEntries {
		var oldest activityKey
		var oldestAt time.Time
		for k, a := range b.pending {
			if oldestAt.IsZero() || a.lastAt.Before(oldestAt) {
				oldest, oldestAt = k, a.lastAt
			}
		}
		delete(b.pending, oldest)
		b.dropped++
	}
}

func fromEvent(e session.Event) *activity {
	a := &activity{
		sessionID: e.SessionID,
		path:      e.Path,
		oldPath:   e.OldPath,
		firstAt:   e.Timestamp,
		lastAt:    e.Timestamp,
		lastOp:    e.Op,
	}
	a.counts.Add(e.Op)
	return a
}

func (b *batcher) save(batch []*activity) error {
	// Apply in time order so version history is chronological.
	sort.Slice(batch, func(i, j int) bool { return batch[i].lastAt.Before(batch[j].lastAt) })

	// Hash once per file, at save time: the content the window ended with.
	type content struct {
		sha256 []byte
		size   int64
	}
	contents := make([]content, len(batch))
	for i, a := range batch {
		if a.lastOp != "REMOVE" {
			if sum, size, ok := fingerprint(a.path); ok {
				contents[i] = content{sum, size}
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), saveTimeout)
	defer cancel()
	if err := b.ensurePartitions(ctx, batch); err != nil {
		return err
	}

	var (
		paths  = make([]string, 0, len(batch))
		hashes [][]byte
		sizes  []int64
		seen   = make(map[string]bool)
	)
	for i, a := range batch {
		paths = append(paths, a.path)
		if a.oldPath != "" {
			paths = append(paths, a.oldPath)
		}
		if c := contents[i]; c.sha256 != nil && !seen[string(c.sha256)] {
			seen[string(c.sha256)] = true
			hashes = append(hashes, c.sha256)
			sizes = append(sizes, c.size)
		}
	}

	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := b.q.WithTx(tx)

	rows, err := q.UpsertFiles(ctx, paths)
	if err != nil {
		return fmt.Errorf("upsert files: %w", err)
	}
	fileIDs := make(map[string]int64, len(rows))
	for _, r := range rows {
		fileIDs[r.Path] = r.ID
	}

	if len(hashes) > 0 {
		if err := q.InsertBlobs(ctx, db.InsertBlobsParams{Hashes: hashes, Sizes: sizes}); err != nil {
			return fmt.Errorf("insert blobs: %w", err)
		}
	}

	rowsToCopy := make([]db.InsertFileActivityParams, 0, len(batch))
	var versions []db.RecordVersionParams
	for i, a := range batch {
		fileID, ok := fileIDs[a.path]
		if !ok {
			return fmt.Errorf("no file id for %q", a.path)
		}
		c := contents[i]

		row := db.InsertFileActivityParams{
			SessionID: a.sessionID,
			FileID:    fileID,
			FirstAt:   a.firstAt,
			LastAt:    a.lastAt,
			Creates:   int32(a.counts.Creates),
			Writes:    int32(a.counts.Writes),
			Renames:   int32(a.counts.Renames),
			Removes:   int32(a.counts.Removes),
			LastOp:    db.FileOp(a.lastOp),
			Sha256:    c.sha256,
		}
		if a.oldPath != "" {
			row.OldPath = &a.oldPath
		}
		if c.sha256 != nil {
			row.SizeBytes = &c.size
		}
		rowsToCopy = append(rowsToCopy, row)

		// A nil hash closes the file's current version: the path a file
		// was renamed away from, or a removed file.
		if a.oldPath != "" {
			versions = append(versions, db.RecordVersionParams{FileID: fileIDs[a.oldPath], At: a.firstAt})
		}
		if a.lastOp == "REMOVE" || c.sha256 != nil {
			versions = append(versions, db.RecordVersionParams{FileID: fileID, Sha256: c.sha256, At: a.lastAt})
		}
	}

	if _, err := q.InsertFileActivity(ctx, rowsToCopy); err != nil {
		return fmt.Errorf("copy activity: %w", err)
	}

	if len(versions) > 0 {
		var versionErr error
		q.RecordVersion(ctx, versions).Exec(func(i int, err error) {
			if err != nil && versionErr == nil {
				versionErr = fmt.Errorf("record version %d: %w", i, err)
			}
		})
		if versionErr != nil {
			return versionErr
		}
	}

	return tx.Commit(ctx)
}

// ensurePartitions creates the daily file_activity partition for each day
// in the batch the first time it's seen. Runs outside the save transaction
// because creating a partition locks the parent table.
func (b *batcher) ensurePartitions(ctx context.Context, batch []*activity) error {
	for _, a := range batch {
		day := a.lastAt.UTC().Format(time.DateOnly)
		if b.partitions[day] {
			continue
		}
		if _, err := b.q.EnsureFileActivityPartition(ctx, a.lastAt); err != nil {
			return fmt.Errorf("ensure partition for %s: %w", day, err)
		}
		b.partitions[day] = true
	}
	return nil
}
