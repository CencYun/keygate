// Package testdb prepares the shared integration test database. It sits
// apart from testsupport because it imports store, and store's own
// internal tests import testsupport.
package testdb

import (
	"fmt"
	"os"
	"testing"

	"github.com/tabloy/keygate/internal/store"
)

// Main runs a package's tests; its TestMain hands over to it. With
// TEST_DATABASE_URL set, it first brings that database to the latest
// schema, so no integration test depends on another one, in this
// package or an earlier one, having run the migrations. It also fails at
// once when the database cannot be reached, where every integration test
// would otherwise skip itself and the run would pass.
func Main(m *testing.M, migrations string) int {
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		s, err := store.New(dsn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "TEST_DATABASE_URL is set but the database is unreachable: %v\n", err)
			return 1
		}
		err = s.RunMigrations(migrations)
		_ = s.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "migrating the test database: %v\n", err)
			return 1
		}
	}
	return m.Run()
}
