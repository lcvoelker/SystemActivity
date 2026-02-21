package session

import (
	"fmt"
	"time"
)

type Manager struct {
	inactivity time.Duration
	current    *Session
	sessions   []Session
	nextID     int
}

func NewManager(inactivity time.Duration) *Manager {
	return &Manager{
		inactivity: inactivity,
		nextID:     1,
	}
}

func (m *Manager) AddEvent(e Event) {
	if m.current == nil {
		m.startNewSession(e)
		return
	}
	if e.Timestamp.Sub(m.current.End) > m.inactivity {
		m.closeCurrentSession()
		m.startNewSession(e)
		return
	}

	m.current.Events = append(m.current.Events, e)
	m.current.End = e.Timestamp
}

func (m *Manager) startNewSession(e Event) {
	m.current = &Session{
		ID:     m.nextID,
		Start:  e.Timestamp,
		End:    e.Timestamp,
		Events: []Event{e},
	}
	m.nextID++
}

func (m *Manager) closeCurrentSession() {
	m.sessions = append(m.sessions, *m.current)
	m.current = nil
	fmt.Printf("Session %d closed: %d events\n",
		m.current.ID,
		len(m.current.Events),
	)
}
