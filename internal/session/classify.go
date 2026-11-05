package session

// Kind is what a session was about. Stored in sessions.kind.
type Kind string

const (
	// KindWork: saving files.
	KindWork Kind = "work"
	// KindOrganization: creating, renaming, moving and deleting files and
	// folders.
	KindOrganization Kind = "organization"
	// KindMixed: both saving and organizing, each a substantial part of the
	// session. Shown as "work + organization".
	KindMixed Kind = "mixed"
	// KindBrief: too few events to tell.
	KindBrief Kind = "brief"
	// KindOther: the old catch-all from before KindMixed and KindBrief.
	// No longer produced; kept so older rows still read.
	KindOther Kind = "other"
)

// Label is the kind as shown to people.
func (k Kind) Label() string {
	if k == KindMixed {
		return "work + organization"
	}
	return string(k)
}

// EndReason is how a session ended. Stored in sessions.end_reason.
type EndReason string

const (
	// EndStopped: the user typed "end" or pressed Ctrl+C.
	EndStopped EndReason = "stopped"
	// EndIdle: the inactivity timeout closed it.
	EndIdle EndReason = "idle"
	// EndRecovered: a later watcher closed it after a kill or failed exit.
	// Its end time is the last saved activity and may be approximate.
	EndRecovered EndReason = "recovered"
)

const (
	// Sessions with fewer events than this are KindBrief.
	minClassifiedEvents = 3
	// Share of a session's events a behavior needs to be in its label, so
	// a stray rename in a long editing session doesn't make it mixed.
	behaviorShare = 0.2
)

// Classify decides a session's kind from how its events split between
// work and organization (see Behavior). Each with at least behaviorShare
// of the events is in the label. Since the two shares add up to 100%, at
// least one always qualifies.
func Classify(b Behavior) Kind {
	total := b.Total()
	if total < minClassifiedEvents {
		return KindBrief
	}
	work := float64(b.Work)/float64(total) >= behaviorShare
	organization := float64(b.Organization)/float64(total) >= behaviorShare
	switch {
	case work && organization:
		return KindMixed
	case work:
		return KindWork
	default:
		return KindOrganization
	}
}
