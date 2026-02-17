package events

import "time"

type EventType int

const (
	Created EventType = iota
	Modified
	Renamed
	Deleted
)

type FileEvent struct {
	Path      string
	Type      EventType
	Timestamp time.Time
}
