package store_test

import (
	"os"
	"testing"

	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

// In the external test package: testdb imports store.
func TestMain(m *testing.M) { os.Exit(testdb.Main(m, "../../db/migrations")) }
