package vec1

import (
	"database/sql"
	"sync"

	_ "github.com/mattn/go-sqlite3"
)

var registerOnce sync.Once

// openOracle opens C SQLite with vec1 registered on every connection
// (sqlite3_auto_extension, through vec1's own VEC1_STATIC entry point).
func openOracle(dsn string) (*sql.DB, error) {
	registerOnce.Do(registerVec1)
	return sql.Open("sqlite3", dsn)
}
