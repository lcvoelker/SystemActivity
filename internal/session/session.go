package session

import "time"

type Event struct {
	SessionID int64
	Path      string
	Op        string
	Timestamp time.Time
	OldPath   string // For rename operations
}

// Counts tallies events by operation. Sessions keep only counts in memory,
// not paths, so a 4–7 hour session over a large tree stays small.
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
	ID     int64
	Start  time.Time
	End    time.Time
	Counts Counts
}

type Summary struct {
	SessionID int64
	Start     time.Time
	End       time.Time
	Counts    Counts
	Kind      Kind
}
