package storage

import (
	"slices"
	"testing"
)

func TestDownDropsListsWhatRollbackDeletes(t *testing.T) {
	got, err := downDrops("00003_session_end_reason.sql")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"DROP VIEW session_summary",
		"ALTER TABLE sessions DROP COLUMN end_reason",
		"DROP TYPE session_end",
	}
	if !slices.Equal(got, want) {
		t.Errorf("downDrops = %q, want %q", got, want)
	}
}

func TestMigrationFilesCoverEveryVersion(t *testing.T) {
	files, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	latest, err := LatestMigration()
	if err != nil {
		t.Fatal(err)
	}
	for v := int64(1); v <= latest; v++ {
		if files[v] == "" {
			t.Errorf("no migration file for version %d", v)
		}
	}
}
