package replication

import (
	"context"
	"fmt"
	"os"
)

// Replicated reports whether the database at path has been opened through
// Open: it carries this package's bookkeeping. It reads the file and writes
// nothing.
func Replicated(ctx context.Context, path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		return false, err
	}
	db, err := openApplyDB(path)
	if err != nil {
		return false, err
	}
	defer db.Close()
	var n int
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM main.sqlite_master WHERE type = 'table' AND name = '_repl_meta'`).Scan(&n)
	return n > 0, err
}

// Check reports what in the ordinary database at path Open would refuse to
// replicate (a trigger, a virtual table, a WITHOUT ROWID table, a key column
// not collated BINARY), naming it, or nil. It reads the file and writes
// nothing, so a caller can check several before it opens any.
func Check(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	db, err := openApplyDB(path)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := catalogRows(ctx, db)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Type == "index" && r.SQL == "" {
			continue // an automatic index is its table's
		}
		if err := supportedObject(ctx, db, r); err != nil {
			return fmt.Errorf("replication: %w", err)
		}
	}
	return nil
}
