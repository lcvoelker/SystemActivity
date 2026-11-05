package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Activity that couldn't be saved before the watcher exited (database down,
// final save failed) is written to a file in the spill directory and loaded
// back into the buffer by the next watcher, which saves it with its next
// regular save. Only a graceful exit can spill; a killed process still
// loses its buffer.

type spilledEntry struct {
	SessionID int64     `json:"session_id"`
	Path      string    `json:"path"`
	OldPath   string    `json:"old_path,omitempty"`
	Creates   int       `json:"creates"`
	Writes    int       `json:"writes"`
	Renames   int       `json:"renames"`
	Removes   int       `json:"removes"`
	FirstAt   time.Time `json:"first_at"`
	LastAt    time.Time `json:"last_at"`
	LastOp    string    `json:"last_op"`
}

// defaultSpillDir is %LOCALAPPDATA%\filemaster\unsaved on Windows. Empty
// (spilling disabled) if the user cache directory can't be determined.
func defaultSpillDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "filemaster", "unsaved")
}

// spill writes entries to a new file in dir and returns its path. The file
// is written under a temporary name and renamed, so a crash mid-write never
// leaves a partial file that looks complete.
func spill(dir string, entries []*activity) (string, error) {
	if dir == "" {
		return "", errors.New("no spill directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	out := make([]spilledEntry, len(entries))
	for i, a := range entries {
		out[i] = spilledEntry{
			SessionID: a.sessionID,
			Path:      a.path,
			OldPath:   a.oldPath,
			Creates:   a.counts.Creates,
			Writes:    a.counts.Writes,
			Renames:   a.counts.Renames,
			Removes:   a.counts.Removes,
			FirstAt:   a.firstAt,
			LastAt:    a.lastAt,
			LastOp:    a.lastOp,
		}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return "", err
	}

	name := filepath.Join(dir, fmt.Sprintf("unsaved-%d-%d.json", time.Now().UnixNano(), os.Getpid()))
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, name); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return name, nil
}

// claimSpilled loads and deletes every spill file in dir. Each file is
// first renamed to a name unique to this process, so two watchers starting
// at once can't both load it. A file that can't be parsed is kept with a
// .bad suffix for inspection.
func claimSpilled(dir string) []*activity {
	if dir == "" {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "unsaved-*.json"))

	var entries []*activity
	for _, m := range matches {
		claimed := fmt.Sprintf("%s.claimed-%d", m, os.Getpid())
		if err := os.Rename(m, claimed); err != nil {
			continue // claimed by another watcher
		}

		var in []spilledEntry
		data, err := os.ReadFile(claimed)
		if err == nil {
			err = json.Unmarshal(data, &in)
		}
		if err != nil {
			log.Printf("storage: can't read unsaved entries in %s: %v", m, err)
			os.Rename(claimed, m+".bad")
			continue
		}

		for _, s := range in {
			a := &activity{
				sessionID: s.SessionID,
				path:      s.Path,
				oldPath:   s.OldPath,
				firstAt:   s.FirstAt,
				lastAt:    s.LastAt,
				lastOp:    s.LastOp,
				recovered: true,
			}
			a.counts.Creates, a.counts.Writes = s.Creates, s.Writes
			a.counts.Renames, a.counts.Removes = s.Renames, s.Removes
			entries = append(entries, a)
		}
		os.Remove(claimed)
	}
	return entries
}
