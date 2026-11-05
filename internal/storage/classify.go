package storage

import (
	"context"
	"fmt"

	"github.com/lcvoelker/SystemActivity/internal/db"
	"github.com/lcvoelker/SystemActivity/internal/session"
)

// classifySaved classifies sessions from their saved per-file activity,
// for sessions the watcher that recorded them didn't close (recovered
// ones). Sessions with no saved activity come out brief.
func classifySaved(ctx context.Context, q *db.Queries, ids []int64) (map[int64]session.Kind, error) {
	rows, err := q.SessionFileCounts(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("read per-file counts: %w", err)
	}
	perSession := make(map[int64][]session.Counts, len(ids))
	for _, r := range rows {
		perSession[r.SessionID] = append(perSession[r.SessionID], session.Counts{
			Creates: int(r.Creates),
			Writes:  int(r.Writes),
			Renames: int(r.Renames),
			Removes: int(r.Removes),
		})
	}
	kinds := make(map[int64]session.Kind, len(ids))
	for _, id := range ids {
		kinds[id] = session.Classify(session.BehaviorFromFiles(perSession[id]))
	}
	return kinds, nil
}
