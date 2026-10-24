package store_test

import (
	"context"
	"os"
	"slices"
	"testing"
)

// The migration that makes max_seats count the owner raises every plan
// that allowed more than one person by one, so no team loses capacity,
// and leaves 1 (a single seat) and 0 (no limit) alone. The file runs as
// written against a temporary table named plans, which the unqualified
// name resolves to first, so the real plans of other tests are untouched.
func TestSeatsIncludeOwnerMigration(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	for _, dir := range []struct {
		file string
		from []int
		want []int
	}{
		{"20261025_seats_include_owner.up.sql", []int{0, 1, 2, 5, 100000}, []int{0, 1, 3, 6, 100001}},
		// A plan at 2 can only have been made after the up: it stays.
		{"20261025_seats_include_owner.down.sql", []int{0, 1, 2, 3, 6, 100001}, []int{0, 1, 2, 2, 5, 100000}},
	} {
		sql, err := os.ReadFile("../../db/migrations/" + dir.file)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, "CREATE TEMP TABLE plans (id int PRIMARY KEY, max_seats int NOT NULL) ON COMMIT DROP"); err != nil {
			t.Fatal(err)
		}
		for i, v := range dir.from {
			if _, err := tx.ExecContext(ctx, "INSERT INTO pg_temp.plans VALUES (?, ?)", i, v); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.ExecContext(ctx, string(sql)); err != nil {
			t.Fatalf("%s: %v", dir.file, err)
		}
		var got []int
		if err := tx.NewRaw("SELECT max_seats FROM pg_temp.plans ORDER BY id").Scan(ctx, &got); err != nil {
			t.Fatal(err)
		}
		_ = tx.Rollback()
		if !slices.Equal(got, dir.want) {
			t.Errorf("%s: %v became %v, want %v", dir.file, dir.from, got, dir.want)
		}
	}
}
