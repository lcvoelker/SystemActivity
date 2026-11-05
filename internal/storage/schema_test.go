package storage

import "testing"

func TestLatestMigrationMatchesFiles(t *testing.T) {
	v, err := LatestMigration()
	if err != nil {
		t.Fatal(err)
	}
	if v != 4 {
		t.Errorf("LatestMigration() = %d, want 4 (update this test when adding a migration)", v)
	}
}

func TestCompareSchemaVersion(t *testing.T) {
	if err := compareSchemaVersion(2, 2); err != nil {
		t.Errorf("matching versions: %v", err)
	}
	if err := compareSchemaVersion(1, 2); err == nil {
		t.Error("pending migration was not reported")
	}
	if err := compareSchemaVersion(3, 2); err == nil {
		t.Error("database newer than build was not reported")
	}
}
