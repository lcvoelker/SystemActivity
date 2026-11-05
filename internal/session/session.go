package session

import "time"

type Event struct {
	SessionID int64
	Path      string
	Op        string
	Timestamp time.Time
	OldPath   string // For rename operations
}

// Counts tallies events by operation.
type Counts struct {
	Creates int
	Writes  int
	Renames int
	Removes int
}

func (c *Counts) Add(op string) {
	switch op {
	case "CREATE":
		c.Creates++
	case "WRITE":
		c.Writes++
	case "RENAME":
		c.Renames++
	case "REMOVE":
		c.Removes++
	}
}

func (c Counts) Total() int {
	return c.Creates + c.Writes + c.Renames + c.Removes
}

type Session struct {
	ID       int64
	Start    time.Time
	End      time.Time
	Counts   Counts
	Files    Files
	behavior behaviorTracker
}

// Behavior is how the session's events split between work and
// organization so far.
func (s *Session) Behavior() Behavior {
	return s.behavior.behavior()
}
