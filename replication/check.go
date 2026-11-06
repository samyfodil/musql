package replication

import (
	"context"
	"errors"
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

// Check reports what in the ordinary database at path Open, in mode (CRDT()
// or Leader(...)), would refuse to replicate, naming it, or nil: a virtual
// table, a key column not collated BINARY, and in CRDT mode a trigger or a
// WITHOUT ROWID table. It reads the file and writes nothing, so a caller can
// check several before it opens any.
func Check(ctx context.Context, path string, mode Option) error {
	var cfg config
	mode(&cfg)
	if cfg.mode == "" {
		return errors.New("replication: Check needs a mode: CRDT() or Leader(isWriter)")
	}
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
		if err := supportedObject(ctx, db, r, cfg.mode != modeCRDT); err != nil {
			return fmt.Errorf("replication: %w", err)
		}
	}
	return nil
}
