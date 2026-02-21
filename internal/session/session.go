package session

import "time"

type Event struct {
	Path      string
	Op        string
	Timestamp time.Time
}

type Session struct {
	ID     int
	Start  time.Time
	End    time.Time
	Events []Event
}
