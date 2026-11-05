package session

import (
	"fmt"
	"hash/maphash"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// maxListedFiles is how many names each operation lists in a summary;
// the rest are counted as "+N more".
const maxListedFiles = 10

var nameSeed = maphash.MakeSeed()

// fileList holds the first maxListedFiles distinct names for one operation
// and an 8-byte hash per further distinct name, so a 7-hour session that
// reorganizes thousands of files stays small in memory.
type fileList struct {
	names    []string
	distinct map[uint64]struct{}
}

func (l *fileList) add(name string) {
	if l.distinct == nil {
		l.distinct = make(map[uint64]struct{})
	}
	h := maphash.String(nameSeed, name)
	if _, ok := l.distinct[h]; ok {
		return
	}
	l.distinct[h] = struct{}{}
	if len(l.names) < maxListedFiles {
		l.names = append(l.names, name)
	}
}

// String lists the names, "a; b; +3 more", or "" if there are none.
func (l fileList) String() string {
	if len(l.names) == 0 {
		return ""
	}
	s := strings.Join(l.names, "; ")
	if more := len(l.distinct) - len(l.names); more > 0 {
		s += fmt.Sprintf("; +%d more", more)
	}
	return s
}

// Files lists, per operation, the distinct files a session touched,
// relative to the watched folder. A rename is listed as "old -> new".
type Files struct {
	Creates fileList
	Writes  fileList
	Renames fileList
	Removes fileList
}

// Add records that op happened to path (renamed from oldPath, if set),
// with paths shown relative to root.
func (f *Files) Add(root, op, path, oldPath string) {
	name := relative(root, path)
	switch op {
	case "CREATE":
		f.Creates.add(name)
	case "WRITE":
		f.Writes.add(name)
	case "RENAME":
		if oldPath != "" {
			name = relative(root, oldPath) + " -> " + name
		}
		f.Renames.add(name)
	case "REMOVE":
		f.Removes.add(name)
	}
}

// AddSaved records one saved activity row (a file's merged counts for one
// save window), for rebuilding a summary from the database.
func (f *Files) AddSaved(root, path, oldPath string, c Counts) {
	if c.Creates > 0 {
		f.Add(root, "CREATE", path, "")
	}
	if c.Writes > 0 {
		f.Add(root, "WRITE", path, "")
	}
	if c.Renames > 0 {
		f.Add(root, "RENAME", path, oldPath)
	}
	if c.Removes > 0 {
		f.Add(root, "REMOVE", path, "")
	}
}

// relative shows path relative to root when it's inside it, else in full.
func relative(root, path string) string {
	if root == "" {
		return path
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return rel
}

// Summary is the printable description of one session.
type Summary struct {
	SessionID int64
	Start     time.Time
	End       time.Time
	Counts    Counts
	Files     Files
	Kind      Kind
	EndReason EndReason // empty while the session is still open
}

// Print writes the summary shown when a session ends and by
// `filemaster session <id>`.
func (s Summary) Print(w io.Writer) {
	fmt.Fprintf(w, "\n=== Session %d summary ===\n", s.SessionID)
	fmt.Fprintf(w, "session type: %s\n", s.Kind.Label())
	fmt.Fprintf(w, "start: %s\nend: %s\n", s.Start.Format(time.RFC3339), s.End.Format(time.RFC3339))
	if s.EndReason != "" {
		fmt.Fprintf(w, "ended by: %s\n", s.EndReason)
	}
	fmt.Fprintf(w, "total events: %d\n", s.Counts.Total())
	for _, line := range []struct {
		op    string
		count int
		files fileList
	}{
		{"CREATE", s.Counts.Creates, s.Files.Creates},
		{"WRITE", s.Counts.Writes, s.Files.Writes},
		{"RENAME", s.Counts.Renames, s.Files.Renames},
		{"REMOVE", s.Counts.Removes, s.Files.Removes},
	} {
		if names := line.files.String(); names != "" {
			fmt.Fprintf(w, "%s: %d (%s)\n", line.op, line.count, names)
		} else {
			fmt.Fprintf(w, "%s: %d\n", line.op, line.count)
		}
	}
	fmt.Fprintln(w, "===========================")
}
