package session

import (
	"context"
	"testing"
	"time"
)

type ending struct {
	kind   Kind
	reason EndReason
}

type fakeStore struct {
	OfflineStore
	ended    map[int64]ending
	recorded []Event
}

func (s *fakeStore) EndSession(_ context.Context, id int64, _ time.Time, kind Kind, reason EndReason) error {
	if s.ended == nil {
		s.ended = make(map[int64]ending)
	}
	s.ended[id] = ending{kind, reason}
	return nil
}

func (s *fakeStore) Record(e Event) { s.recorded = append(s.recorded, e) }

func TestManagerSplitsSessionsOnInactivity(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	m := NewManager("", time.Minute, store)
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

	if got := store.ended[1]; got != (ending{KindWork, EndIdle}) {
		t.Fatalf("session 1 ended as %+v, want work closed by idle timeout", got)
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
	if summary.SessionID != 2 || summary.Counts.Total() != 1 || summary.Kind != KindBrief {
		t.Errorf("summary = %+v, want session 2 with 1 event, kind brief", summary)
	}
	if m.CurrentSession() != nil {
		t.Error("session still open after EndCurrentSession")
	}
	if got := store.ended[2]; got.reason != EndStopped {
		t.Errorf("session 2 ended with reason %q, want stopped", got.reason)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		b    Behavior
		want Kind
	}{
		{"frequent saves", Behavior{Work: 40, Organization: 2}, KindWork},
		{"stray rename in long editing session", Behavior{Work: 40, Organization: 1}, KindWork},
		{"reorganizing", Behavior{Work: 3, Organization: 45}, KindOrganization},
		{"saving and tidying (session 2 of 2026-11-05)", Behavior{Work: 4, Organization: 3}, KindMixed},
		{"half and half", Behavior{Work: 10, Organization: 10}, KindMixed},
		{"organizing just at 20%", Behavior{Work: 8, Organization: 2}, KindMixed},
		{"organizing just under 20%", Behavior{Work: 9, Organization: 2}, KindWork},
		{"too little", Behavior{Work: 2}, KindBrief},
		{"empty", Behavior{}, KindBrief},
	}
	for _, tt := range tests {
		if got := Classify(tt.b); got != tt.want {
			t.Errorf("%s: Classify(%+v) = %s, want %s", tt.name, tt.b, got, tt.want)
		}
	}
}

// New files that get saved are work, including their creation and renames.
func TestBehaviorCountsSavedNewFilesAsWork(t *testing.T) {
	type ev struct{ op, path, old string }
	tests := []struct {
		name   string
		events []ev
		want   Behavior
	}{
		{"create then save 4 times", []ev{
			{"CREATE", "doc", ""}, {"WRITE", "doc", ""}, {"WRITE", "doc", ""}, {"WRITE", "doc", ""}, {"WRITE", "doc", ""},
		}, Behavior{Work: 5}},
		{"create, save, rename the new file", []ev{
			{"CREATE", "draft", ""}, {"WRITE", "draft", ""}, {"RENAME", "final", "draft"},
		}, Behavior{Work: 3}},
		{"create, rename, then save under the new name", []ev{
			{"CREATE", "untitled", ""}, {"RENAME", "essay", "untitled"}, {"WRITE", "essay", ""},
		}, Behavior{Work: 3}},
		{"new file never saved", []ev{
			{"CREATE", "folder", ""}, {"RENAME", "archive", "folder"},
		}, Behavior{Organization: 2}},
		{"scratch file created and deleted unsaved", []ev{
			{"CREATE", "tmp", ""}, {"REMOVE", "tmp", ""},
		}, Behavior{Organization: 2}},
		{"renaming a file that existed before the session", []ev{
			{"RENAME", "new", "old"}, {"WRITE", "new", ""},
		}, Behavior{Work: 1, Organization: 1}},
	}
	for _, tt := range tests {
		var tr behaviorTracker
		for _, e := range tt.events {
			tr.add(e.op, e.path, e.old)
		}
		if got := tr.behavior(); got != tt.want {
			t.Errorf("%s: behavior = %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

func TestBehaviorFromSavedFiles(t *testing.T) {
	got := BehaviorFromFiles([]Counts{
		{Creates: 1, Writes: 4, Renames: 1}, // new document, saved and renamed: work
		{Creates: 1},                        // new folder: organization
		{Renames: 2, Writes: 1},             // existing file renamed and saved
		{Removes: 1},
	})
	if want := (Behavior{Work: 7, Organization: 4}); got != want {
		t.Errorf("BehaviorFromFiles = %+v, want %+v", got, want)
	}
}

func TestManagerClassifiesNewDocumentAsWork(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	m := NewManager("", time.Hour, store)
	now := time.Now()
	for i, e := range []Event{
		{Op: "CREATE", Path: "report.docx"},
		{Op: "WRITE", Path: "report.docx"},
		{Op: "WRITE", Path: "report.docx"},
		{Op: "WRITE", Path: "report.docx"},
		{Op: "WRITE", Path: "report.docx"},
	} {
		e.Timestamp = now.Add(time.Duration(i) * time.Second)
		if err := m.AddEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := m.EndCurrentSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Kind != KindWork {
		t.Errorf("create + 4 saves of one document = %s, want work", summary.Kind)
	}
}

func TestMixedIsLabeledWorkPlusOrganization(t *testing.T) {
	if got := KindMixed.Label(); got != "work + organization" {
		t.Errorf("KindMixed.Label() = %q", got)
	}
	if got := KindWork.Label(); got != "work" {
		t.Errorf("KindWork.Label() = %q", got)
	}
}
