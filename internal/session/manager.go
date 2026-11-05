package session

import (
	"context"
	"fmt"
	"os"
	"time"
)

// Store persists sessions and their events. StartSession and EndSession are
// synchronous so events always reference a session that exists; Record may
// buffer.
type Store interface {
	StartSession(ctx context.Context, start time.Time) (int64, error)
	EndSession(ctx context.Context, id int64, end time.Time, kind Kind, reason EndReason) error
	Record(e Event)
}

type Manager struct {
	root       string // watched folder; summaries show paths relative to it
	inactivity time.Duration
	store      Store
	current    *Session
}

func NewManager(root string, inactivity time.Duration, store Store) *Manager {
	return &Manager{
		root:       root,
		inactivity: inactivity,
		store:      store,
	}
}

func (m *Manager) AddEvent(ctx context.Context, e Event) error {
	if m.current != nil && e.Timestamp.Sub(m.current.End) > m.inactivity {
		if err := m.closeCurrentSession(ctx, EndIdle); err != nil {
			return err
		}
	}
	if m.current == nil {
		if err := m.startNewSession(ctx, e.Timestamp); err != nil {
			return err
		}
	}

	e.SessionID = m.current.ID
	m.current.End = e.Timestamp
	m.current.Counts.Add(e.Op)
	m.current.Files.Add(m.root, e.Op, e.Path, e.OldPath)
	m.current.behavior.add(e.Op, e.Path, e.OldPath)
	m.store.Record(e)
	return nil
}

func (m *Manager) startNewSession(ctx context.Context, start time.Time) error {
	id, err := m.store.StartSession(ctx, start)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	m.current = &Session{ID: id, Start: start, End: start}
	return nil
}

// closeCurrentSession always clears the current session, even if the store
// fails, so the next event starts a fresh one.
func (m *Manager) closeCurrentSession(ctx context.Context, reason EndReason) error {
	if m.current == nil {
		return nil
	}
	s := m.current
	m.current = nil

	kind := Classify(s.Behavior())
	fmt.Printf("Session %d closed (%s): %d events, %s\n", s.ID, reason, s.Counts.Total(), kind.Label())
	if err := m.store.EndSession(ctx, s.ID, s.End, kind, reason); err != nil {
		return fmt.Errorf("end session %d: %w", s.ID, err)
	}
	return nil
}

func (m *Manager) CurrentSession() *Session {
	return m.current
}

func (m *Manager) EndCurrentSession(ctx context.Context) (Summary, error) {
	if m.current == nil {
		return Summary{}, nil
	}

	s := *m.current
	summary := Summary{
		SessionID: s.ID,
		Start:     s.Start,
		End:       s.End,
		Counts:    s.Counts,
		Files:     s.Files,
		Kind:      Classify(s.Behavior()),
		EndReason: EndStopped,
	}
	summary.Print(os.Stdout)

	return summary, m.closeCurrentSession(ctx, EndStopped)
}

// OfflineStore numbers sessions locally and discards events. Used when the
// watcher runs without a database.
type OfflineStore struct {
	nextID int64
}

func (s *OfflineStore) StartSession(context.Context, time.Time) (int64, error) {
	s.nextID++
	return s.nextID, nil
}

func (s *OfflineStore) EndSession(context.Context, int64, time.Time, Kind, EndReason) error {
	return nil
}

func (s *OfflineStore) Record(Event) {}
