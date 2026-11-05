package session

import "hash/maphash"

// Behavior splits a session's events into work and organization for
// classification. It differs from raw Counts in one way: a file created
// during the session and then saved is a new document being worked on, so
// its CREATE, and any renames of it (draft.txt -> final.txt), count as
// work. A file only created, renamed, moved or deleted stays organization.
type Behavior struct {
	Work         int
	Organization int
}

func (b Behavior) Total() int { return b.Work + b.Organization }

// newFile is a file created during the current session, followed across
// renames by its current path.
type newFile struct {
	pending int  // its CREATE and renames, not yet known to be work
	written bool // saved at least once since it was created
}

// behaviorTracker attributes events to work or organization as they
// arrive. It keeps one small entry per file created in the session.
type behaviorTracker struct {
	Behavior
	created map[uint64]*newFile
}

func (t *behaviorTracker) add(op, path, oldPath string) {
	if t.created == nil {
		t.created = make(map[uint64]*newFile)
	}
	key := maphash.String(nameSeed, path)
	switch op {
	case "CREATE":
		t.created[key] = &newFile{pending: 1}
	case "WRITE":
		if f := t.created[key]; f != nil && !f.written {
			t.Work += f.pending // the new file was being worked on all along
			f.pending, f.written = 0, true
		}
		t.Work++
	case "RENAME":
		oldKey := maphash.String(nameSeed, oldPath)
		f := t.created[oldKey]
		if oldPath == "" || f == nil {
			t.Organization++
			return
		}
		delete(t.created, oldKey)
		t.created[key] = f
		if f.written {
			t.Work++
		} else {
			f.pending++
		}
	case "REMOVE":
		if f := t.created[key]; f != nil {
			// Created and deleted again unsaved (e.g. a scratch file): its
			// create and renames were organization after all.
			t.Organization += f.pending
			delete(t.created, key)
		}
		t.Organization++
	}
}

// behavior is the split so far. Created files never saved still count as
// organization.
func (t *behaviorTracker) behavior() Behavior {
	b := t.Behavior
	for _, f := range t.created {
		b.Organization += f.pending
	}
	return b
}

// BehaviorFromFiles applies the same rule to saved per-file totals, for
// sessions classified from the database (recovered sessions, or open
// ones). It sees each path separately, so a new file renamed before its
// first save is counted as organization; the live tracker follows it.
func BehaviorFromFiles(files []Counts) Behavior {
	var b Behavior
	for _, c := range files {
		if c.Creates > 0 && c.Writes > 0 {
			b.Work += c.Creates + c.Writes + c.Renames
			b.Organization += c.Removes
			continue
		}
		b.Work += c.Writes
		b.Organization += c.Creates + c.Renames + c.Removes
	}
	return b
}
