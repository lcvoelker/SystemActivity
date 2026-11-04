package session

// Kind is what a session was mostly about. Stored in sessions.kind.
type Kind string

const (
	// KindWork: mostly saves to existing files.
	KindWork Kind = "work"
	// KindOrganization: mostly creating, renaming, moving and deleting
	// files and folders.
	KindOrganization Kind = "organization"
	// KindOther: mixed activity, or too little to tell.
	KindOther Kind = "other"
)

const (
	// Sessions with fewer events than this are KindOther.
	minClassifiedEvents = 3
	// Share of events one side needs to decide the kind.
	dominantShare = 0.6
)

// Classify decides a session's kind from its operation counts. Writes
// signal work; creates, renames (including moves) and removes signal
// organization.
func Classify(c Counts) Kind {
	total := c.Total()
	if total < minClassifiedEvents {
		return KindOther
	}
	structural := c.Creates + c.Renames + c.Removes
	switch {
	case float64(c.Writes)/float64(total) >= dominantShare:
		return KindWork
	case float64(structural)/float64(total) >= dominantShare:
		return KindOrganization
	default:
		return KindOther
	}
}
