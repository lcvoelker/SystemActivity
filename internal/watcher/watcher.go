package watcher

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"main/internal/events"
	"main/internal/filter"
)

type Watcher struct {
	fsWatcher *fsnotify.Watcher
	Out       chan events.FileEvent
}

func New() (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	return &Watcher{
		fsWatcher: fsw,
		Out:       make(chan events.FileEvent, 100)}, nil

}

func (w *Watcher) AddRecursive(root string) error {
	if _, err := os.Stat(root); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return w.fsWatcher.Add(path)
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
		_ = w.fsWatcher.Add(path)
	}
}

func (w *Watcher) Start() {
	go func() {
		for {
			select {
			case ev := <-w.fsWatcher.Events:
				if ev.Op&fsnotify.Create != 0 {
					w.addIfDir(ev.Name)
				}
				if e, ok := normalize(ev); ok {
					if filter.ShouldIgnore(e.Path) {
						continue
					}
					w.Out <- e
				}
			case err := <-w.fsWatcher.Errors:
				log.Println("watcher error: ", err)
			}
		}
	}()
}

func normalize(ev fsnotify.Event) (events.FileEvent, bool) {
	now := time.Now()

	switch {
	case ev.Op&fsnotify.Chmod != 0:
		return events.FileEvent{}, false
	case ev.Op&fsnotify.Create != 0:
		return events.FileEvent{Path: ev.Name, Type: events.Created, Timestamp: now}, true
	case ev.Op&fsnotify.Write != 0:
		return events.FileEvent{Path: ev.Name, Type: events.Modified, Timestamp: now}, true
	case ev.Op&fsnotify.Rename != 0:
		return events.FileEvent{Path: ev.Name, Type: events.Renamed, Timestamp: now}, true
	case ev.Op&fsnotify.Remove != 0:
		return events.FileEvent{Path: ev.Name, Type: events.Deleted, Timestamp: now}, true
	default:
		return events.FileEvent{}, false
	}
}
