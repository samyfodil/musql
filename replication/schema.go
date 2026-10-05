package replication

import (
	"context"
	"database/sql"
	"strings"
)

const (
	// _repl_meta holds bookkeeping: site id, schema fingerprint, hlc state
	_repl_meta = `CREATE TABLE IF NOT EXISTS main._repl_meta(
		k TEXT PRIMARY KEY,
		v BLOB
	)`

	// _repl_oplog is the durable history of all operations (grow-only)
	_repl_oplog = `CREATE TABLE IF NOT EXISTS main._repl_oplog(
		site TEXT,
		seq INTEGER,
		hlc INTEGER,
		tbl TEXT,
		pk BLOB,
		op INTEGER,
		cells BLOB,
		PRIMARY KEY(site,seq)
	)`

	// _repl_clock holds per-cell last-writer-wins state
	_repl_clock = `CREATE TABLE IF NOT EXISTS main._repl_clock(
		tbl TEXT,
		pk BLOB,
		col TEXT,
		hlc INTEGER,
		site TEXT,
		val BLOB,
		PRIMARY KEY(tbl,pk,col)
	)`

	// _repl_ranges is which site owns each rowid range (RowidRange's lo):
	// the first one this node admitted.
	_repl_ranges = `CREATE TABLE IF NOT EXISTS main._repl_ranges(
		lo INTEGER PRIMARY KEY,
		site TEXT NOT NULL
	)`

	// _repl_withheld is the rows the clock has alive and complete that the
	// user table does not, because a UNIQUE value they hold went to a newer row
	// (Store.rebuildTable).
	// _repl_floors is, per site, the seq its ops are pruned up to: every op at
	// or below it has been applied everywhere, and only the schema ops among
	// them are still in _repl_oplog.
	_repl_floors = `CREATE TABLE IF NOT EXISTS main._repl_floors(
		site TEXT PRIMARY KEY,
		seq INTEGER NOT NULL
	)`

	_repl_withheld = `CREATE TABLE IF NOT EXISTS main._repl_withheld(
		tbl TEXT,
		pk BLOB,
		PRIMARY KEY(tbl, pk)
	)`
)

// bookkeeping is this package's own tables. Only these exact names are
// skipped by capture and left out of the replicated schema: a user table that
// merely starts with "_repl_" replicates like any other.
var bookkeeping = []string{"_repl_meta", "_repl_oplog", "_repl_clock", "_repl_ranges", "_repl_withheld", "_repl_floors"}

func isBookkeeping(name string) bool {
	for _, b := range bookkeeping {
		if fold(name) == b {
			return true
		}
	}
	return false
}

// InitSchema creates the bookkeeping tables if they do not exist, first
// renaming a database's "_crdt_*" tables -- what this package called them
// when it was named crdt -- to their current names. It is idempotent.
func InitSchema(ctx context.Context, db *sql.DB) error {
	for _, b := range bookkeeping {
		old := "_crdt_" + strings.TrimPrefix(b, "_repl_")
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM main.sqlite_master WHERE type='table' AND name=?`, old).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			if _, err := db.ExecContext(ctx, `ALTER TABLE main.`+old+` RENAME TO `+b); err != nil {
				return err
			}
		}
	}
	for _, ddl := range []string{_repl_meta, _repl_oplog, _repl_clock, _repl_ranges, _repl_withheld, _repl_floors} {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	return nil
}
