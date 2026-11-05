package storage

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lcvoelker/SystemActivity/internal/db"
	"github.com/lcvoelker/SystemActivity/internal/session"
)

const (
	// Everything older than this is deleted. Agreed with the project owner;
	// ask before changing (see AGENTS.md).
	retention = 3 * 24 * time.Hour
	// How often the watcher applies retention while running.
	pruneInterval = time.Hour
)

// Store implements session.Store on PostgreSQL. Session rows are written
// immediately; file activity goes through a batcher.
type Store struct {
	q       *db.Queries
	host    string
	root    string
	batcher *batcher
}

// NewStore starts a store whose unsaved activity is kept in the user's
// cache directory on exit and recovered on the next start.
func NewStore(pool *pgxpool.Pool, root string) *Store {
	return newStore(pool, root, defaultSpillDir())
}

func newStore(pool *pgxpool.Pool, root, spillDir string) *Store {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	q := db.New(pool)
	return &Store{
		q:       q,
		host:    host,
		root:    root,
		batcher: newBatcher(pool, q, spillDir),
	}
}

func (s *Store) StartSession(ctx context.Context, start time.Time) (int64, error) {
	return s.q.StartSession(ctx, db.StartSessionParams{
		Host:      s.host,
		RootPath:  s.root,
		StartedAt: start,
	})
}

func (s *Store) EndSession(ctx context.Context, id int64, end time.Time, kind session.Kind, reason session.EndReason) error {
	return s.q.EndSession(ctx, db.EndSessionParams{
		ID:        id,
		EndedAt:   end,
		Kind:      db.SessionKind(kind),
		EndReason: db.SessionEnd(reason),
	})
}

func (s *Store) Record(e session.Event) {
	s.batcher.add(e)
}

// Close saves buffered activity. Call it before closing the pool.
func (s *Store) Close() {
	s.batcher.close()
}

// CloseOrphans ends sessions on this host and root that are still open
// because a previous watcher was killed. Each is closed at its last saved
// activity and classified from what reached the database; activity that
// was still buffered in the killed process is lost.
func (s *Store) CloseOrphans(ctx context.Context) (int, error) {
	open, err := s.q.OpenSessions(ctx, db.OpenSessionsParams{Host: s.host, RootPath: s.root})
	if err != nil || len(open) == 0 {
		return 0, err
	}
	ids := make([]int64, len(open))
	for i, o := range open {
		ids[i] = o.SessionID
	}
	kinds, err := classifySaved(ctx, s.q, ids)
	if err != nil {
		return 0, err
	}
	for _, o := range open {
		if err := s.EndSession(ctx, o.SessionID, o.LastSeenAt, kinds[o.SessionID], session.EndRecovered); err != nil {
			return 0, fmt.Errorf("close orphaned session %d: %w", o.SessionID, err)
		}
	}
	return len(open), nil
}

// PruneExpired deletes everything older than the retention period.
func PruneExpired(ctx context.Context, q *db.Queries) (db.PruneBeforeRow, error) {
	return q.PruneBefore(ctx, time.Now().Add(-retention))
}

// RunRetention applies the retention policy now and every pruneInterval
// until ctx is cancelled.
func (s *Store) RunRetention(ctx context.Context) {
	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()
	for {
		res, err := PruneExpired(ctx, s.q)
		switch {
		case err != nil && ctx.Err() == nil:
			log.Printf("storage: retention failed: %v", err)
		case err == nil && (res.DroppedPartitions > 0 || res.DeletedSessions > 0):
			log.Printf("storage: retention removed %d day partitions and %d sessions older than %s",
				res.DroppedPartitions, res.DeletedSessions, retention)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
