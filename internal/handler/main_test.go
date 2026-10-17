package handler

import (
	"os"
	"testing"

	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m, "../../db/migrations")) }
