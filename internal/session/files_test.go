package session

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The summary from the 2026-11-05 example: saving and tidying in one session.
func TestSummaryListsFilesPerOperation(t *testing.T) {
	root := filepath.Join(`D:\`, "Downloads", "look")
	in := func(name string) string { return filepath.Join(root, name) }

	var s Session
	for _, e := range []Event{
		{Op: "RENAME", OldPath: in("essayDraft.txt"), Path: in("essayFinal.txt")},
		{Op: "RENAME", OldPath: in("test1.pdf"), Path: in("TEST.pdf")},
		{Op: "REMOVE", Path: in("proposal.txt")},
		{Op: "WRITE", Path: in("pass2.pdf")},
		{Op: "WRITE", Path: in("resume1.pdf")},
		{Op: "WRITE", Path: in("resume1.pdf")}, // second save: listed once
		{Op: "WRITE", Path: in("resume7.pdf")},
	} {
		s.Counts.Add(e.Op)
		s.Files.Add(root, e.Op, e.Path, e.OldPath)
		s.behavior.add(e.Op, e.Path, e.OldPath)
	}

	var out bytes.Buffer
	Summary{
		SessionID: 2,
		Start:     time.Date(2026, 11, 5, 14, 2, 33, 0, time.UTC),
		End:       time.Date(2026, 11, 5, 14, 4, 16, 0, time.UTC),
		Counts:    s.Counts,
		Files:     s.Files,
		Kind:      Classify(s.Behavior()),
	}.Print(&out)

	for _, want := range []string{
		"session type: work + organization\n",
		"total events: 7\n",
		"CREATE: 0\n",
		"WRITE: 4 (pass2.pdf; resume1.pdf; resume7.pdf)\n",
		"RENAME: 2 (essayDraft.txt -> essayFinal.txt; test1.pdf -> TEST.pdf)\n",
		"REMOVE: 1 (proposal.txt)\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary missing %q:\n%s", want, out.String())
		}
	}
}

func TestFileListCapsNamesAndCountsTheRest(t *testing.T) {
	var l fileList
	for i := range 25 {
		l.add(fmt.Sprintf("f%02d.txt", i))
		l.add(fmt.Sprintf("f%02d.txt", i)) // repeats don't count
	}
	got := l.String()
	if !strings.HasPrefix(got, "f00.txt; f01.txt;") || !strings.HasSuffix(got, "f09.txt; +15 more") {
		t.Errorf("fileList = %q, want the first 10 names then +15 more", got)
	}
}

func TestSubfolderPathsAreRelativeToRoot(t *testing.T) {
	root := filepath.Join(`D:\`, "look")
	var f Files
	f.Add(root, "WRITE", filepath.Join(root, "2026", "taxes.xlsx"), "")
	f.Add(root, "RENAME", filepath.Join(root, "archive", "a.pdf"), filepath.Join(root, "a.pdf"))
	f.Add(root, "REMOVE", filepath.Join(`D:\`, "elsewhere", "x.txt"), "")

	if got, want := f.Writes.String(), filepath.Join("2026", "taxes.xlsx"); got != want {
		t.Errorf("writes = %q, want %q", got, want)
	}
	if got, want := f.Renames.String(), "a.pdf -> "+filepath.Join("archive", "a.pdf"); got != want {
		t.Errorf("renames = %q, want %q (a move into a subfolder)", got, want)
	}
	if got, want := f.Removes.String(), filepath.Join(`D:\`, "elsewhere", "x.txt"); got != want {
		t.Errorf("path outside root = %q, want it in full", got)
	}
}
