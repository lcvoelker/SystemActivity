package watcher

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/lcvoelker/SystemActivity/internal/filter"
	"github.com/lcvoelker/SystemActivity/internal/session"
)

// Per-directory change buffer. fsnotify's default is 64 KB; 16 KB cuts
// memory on large trees to about 16 MB per 1,000 folders, at the risk of
// an overflow (logged) during a very large burst in one folder. Agreed with
// the project owner; ask before changing (see AGENTS.md).
const watchBufferSize = 16 * 1024

type Watcher struct {
	fsWatcher      *fsnotify.Watcher
	pendingRenames map[string]string // oldPath -> timestamp (simplified approach)
	renameCounter  int               // To track rename sequence
	started        time.Time
}

func New() (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	return &Watcher{
		fsWatcher:      fsw,
		pendingRenames: make(map[string]string),
		renameCounter:  0,
		started:        time.Now(),
	}, nil

}

// AddRecursive watches root and every folder below it. Files are not read
// or recorded here: only files that change while the watcher runs are.
func (w *Watcher) AddRecursive(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return w.fsWatcher.AddWith(path, fsnotify.WithBufferSize(watchBufferSize))
		}
		return nil
	})
}

func (w *Watcher) addIfDir(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.IsDir() {
		_ = w.fsWatcher.AddWith(path, fsnotify.WithBufferSize(watchBufferSize))
	}
}

func (w *Watcher) Start(out chan<- session.Event) {
	log.Println("Watcher started. Type \"end\" to stop and print the session events summary.")

	// Cleanup old pending renames periodically
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		for {
			select {
			case <-ticker.C:
				w.cleanupOldRenames()
			}
		}
	}()

	for {
		select {
		case ev := <-w.fsWatcher.Events:
			if ev.Op&fsnotify.Create != 0 {
				w.addIfDir(ev.Name)
			}
			if e, ok := w.normalize(ev); ok {
				if filter.ShouldIgnore(e.Path) {
					continue
				}
				out <- e
			}
		case err := <-w.fsWatcher.Errors:
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				log.Println("watcher error: change buffer overflowed, some events were lost")
				continue
			}
			log.Println("watcher error: ", err)
		}
	}
}

func (w *Watcher) normalize(ev fsnotify.Event) (session.Event, bool) {
	now := time.Now()

	switch {
	case ev.Op&fsnotify.Chmod != 0:
		return session.Event{}, false
	case ev.Op&fsnotify.Create != 0:
		// Check if this CREATE matches a pending RENAME: the most recent RENAME is likely the source
		if len(w.pendingRenames) > 0 {
			// Get the most recent rename FIFO
			for oldPath := range w.pendingRenames {
				delete(w.pendingRenames, oldPath)
				return session.Event{Path: ev.Name, Op: "RENAME", Timestamp: now, OldPath: oldPath}, true
			}
		}
		return session.Event{Path: ev.Name, Op: "CREATE", Timestamp: now}, true
	case ev.Op&fsnotify.Write != 0:
		if !w.modifiedSinceStart(ev.Name) {
			return session.Event{}, false
		}
		return session.Event{Path: ev.Name, Op: "WRITE", Timestamp: now}, true
	case ev.Op&fsnotify.Rename != 0:
		// Store the rename as pending - this is the old path
		w.pendingRenames[ev.Name] = fmt.Sprintf("pending_%d", w.renameCounter)
		w.renameCounter++
		return session.Event{}, false // Don't emit the RENAME event yet
	case ev.Op&fsnotify.Remove != 0:
		return session.Event{Path: ev.Name, Op: "REMOVE", Timestamp: now}, true
	default:
		return session.Event{}, false
	}
}

// modifiedSinceStart filters WRITE events that aren't real saves:
//   - folders, which report a write whenever their contents change (those
//     changes arrive as their own events);
//   - files whose modified time predates the watcher, i.e. touched by
//     antivirus, sync clients or indexers without being changed.
//
// If the file can't be inspected (e.g. already deleted) the event is kept.
func (w *Watcher) modifiedSinceStart(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return true
	}
	return !info.IsDir() && !info.ModTime().Before(w.started)
}

func (w *Watcher) cleanupOldRenames() {
	// remove all pending renames older than 1 minute
	if len(w.pendingRenames) > 100 { // Prevent memory leak
		w.pendingRenames = make(map[string]string)
	}
}
