package storage

import (
	"context"
	"os"
	"sync"
	"testing"
)

// Rolls the test database back one migration, then starts two
// connections at once: the advisory lock must let exactly one of them
// apply it, and both must end up with a working app-role pool. Needs an
// owner connection, so it runs only when FILEMASTER_TEST_MIGRATE_DATABASE_URL
// is also set (never MIGRATE_DATABASE_URL or .env).
func TestConnectAppliesPendingMigrationsOnce(t *testing.T) {
	appDSN := os.Getenv("FILEMASTER_TEST_DATABASE_URL")
	ownerDSN := os.Getenv("FILEMASTER_TEST_MIGRATE_DATABASE_URL")
	if appDSN == "" || ownerDSN == "" {
		t.Skip("FILEMASTER_TEST_DATABASE_URL and FILEMASTER_TEST_MIGRATE_DATABASE_URL not both set")
	}
	ctx := context.Background()
	latest, err := LatestMigration()
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, ownerDSN, "down"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Migrate(ctx, ownerDSN, "up") })

	// Without owner credentials the schema stays behind and Connect refuses.
	if _, _, err := Connect(ctx, appDSN, ""); err == nil {
		t.Fatal("Connect without MIGRATE_DATABASE_URL accepted an out-of-date schema")
	}

	var wg sync.WaitGroup
	applied := make([][]int64, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool, a, err := Connect(ctx, appDSN, ownerDSN)
			applied[i], errs[i] = a, err
			if pool != nil {
				pool.Close()
			}
		}()
	}
	wg.Wait()

	total := 0
	for i := range 2 {
		if errs[i] != nil {
			t.Errorf("connection %d: %v", i, errs[i])
		}
		total += len(applied[i])
		for _, v := range applied[i] {
			if v != latest {
				t.Errorf("connection %d applied version %d, want %d", i, v, latest)
			}
		}
	}
	if total != 1 {
		t.Errorf("migration applied %d times across both connections, want exactly once: %v", total, applied)
	}
}
