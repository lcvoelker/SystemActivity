package session

import (
	"context"
	"testing"
	"time"
)

type fakeStore struct {
	OfflineStore
	ended    map[int64]Kind
	recorded []Event
}

func (s *fakeStore) EndSession(_ context.Context, id int64, _ time.Time, kind Kind) error {
	if s.ended == nil {
		s.ended = make(map[int64]Kind)
	}
	s.ended[id] = kind
	return nil
}

func (s *fakeStore) Record(e Event) { s.recorded = append(s.recorded, e) }

func TestManagerSplitsSessionsOnInactivity(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	m := NewManager(time.Minute, store)
	t0 := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)

	for _, e := range []Event{
		{Path: "a", Op: "WRITE", Timestamp: t0},
		{Path: "a", Op: "WRITE", Timestamp: t0.Add(10 * time.Second)},
		{Path: "a", Op: "WRITE", Timestamp: t0.Add(30 * time.Second)},
		{Path: "b", Op: "CREATE", Timestamp: t0.Add(5 * time.Minute)}, // gap > 1m
	} {
		if err := m.AddEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	if kind, ok := store.ended[1]; !ok || kind != KindWork {
		t.Fatalf("session 1 ended = %v (%q), want work", ok, kind)
	}
	wantIDs := []int64{1, 1, 1, 2}
	for i, e := range store.recorded {
		if e.SessionID != wantIDs[i] {
			t.Errorf("event %d session = %d, want %d", i, e.SessionID, wantIDs[i])
		}
	}

	summary, err := m.EndCurrentSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.SessionID != 2 || summary.Counts.Total() != 1 || summary.Kind != KindOther {
		t.Errorf("summary = %+v, want session 2 with 1 event, kind other", summary)
	}
	if m.CurrentSession() != nil {
		t.Error("session still open after EndCurrentSession")
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name   string
		counts Counts
		want   Kind
	}{
		{"frequent saves", Counts{Writes: 40, Creates: 2}, KindWork},
		{"reorganizing", Counts{Renames: 30, Creates: 10, Removes: 5, Writes: 3}, KindOrganization},
		{"mixed", Counts{Writes: 10, Renames: 10}, KindOther},
		{"too little", Counts{Writes: 2}, KindOther},
		{"empty", Counts{}, KindOther},
	}
	for _, tt := range tests {
		if got := Classify(tt.counts); got != tt.want {
			t.Errorf("%s: Classify(%+v) = %s, want %s", tt.name, tt.counts, got, tt.want)
		}
	}
}
