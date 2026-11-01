package replication

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
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

	// _repl_clock holds each row's last-writer-wins state: its cells, packed
	// into one record (clockrec.go)
	_repl_clock = `CREATE TABLE IF NOT EXISTS main._repl_clock(
		tbl TEXT,
		pk BLOB,
		top INTEGER,
		rec BLOB,
		PRIMARY KEY(tbl,pk)
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
// when it was named crdt -- to their current names, and packs a clock of a row
// per cell into records (clockLayout). It is idempotent.
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
	var cells bool
	if err := db.QueryRowContext(ctx, clockHasCells).Scan(&cells); err != nil {
		return err
	}
	for _, ddl := range []string{_repl_meta, _repl_oplog, _repl_clock, _repl_ranges, _repl_withheld, _repl_floors} {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	var v string
	err := db.QueryRowContext(ctx, `SELECT v FROM main._repl_meta WHERE k=?`, metaLayout).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows) && cells:
		return packClock(ctx, db)
	case errors.Is(err, sql.ErrNoRows):
		_, err = db.ExecContext(ctx, `INSERT INTO main._repl_meta(k, v) VALUES(?, ?) ON CONFLICT(k) DO NOTHING`, metaLayout, strconv.Itoa(clockLayout))
		return err
	case err != nil:
		return err
	}
	if n, perr := strconv.Atoi(v); perr != nil || n != clockLayout || cells {
		return fmt.Errorf("replication: bookkeeping layout %q, which this build does not read (it reads %d)", v, clockLayout)
	}
	return nil
}

// clockLayout is the _repl_clock layout this build reads and writes, kept in
// _repl_meta under metaLayout: one record per row. A database without it holds
// a row per cell, which InitSchema packs once; an older build fails at
// OpenStore rather than misread the clock.
const (
	metaLayout  = "layout"
	clockLayout = 2
	// clockHasCells asks whether _repl_clock holds a row per cell.
	clockHasCells = `SELECT count(*) > 0 FROM pragma_table_info('_repl_clock', 'main') WHERE name='hlc'`
)

// packClock moves a clock of a row per cell into records, in one transaction.
func packClock(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	if err := packClockIn(ctx, conn); err != nil {
		conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
		return err
	}
	_, err = conn.ExecContext(ctx, `COMMIT`)
	return err
}

func packClockIn(ctx context.Context, conn *sql.Conn) error {
	var cells bool
	if err := conn.QueryRowContext(ctx, clockHasCells).Scan(&cells); err != nil || !cells {
		return err // another connection packed it first
	}
	if _, err := conn.ExecContext(ctx, `ALTER TABLE main._repl_clock RENAME TO _repl_clock_cells`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, _repl_clock); err != nil {
		return err
	}
	// Keys go in order, a page of cells at a time; the last key of a full page
	// may go on past it, and is read again whole with the next.
	tbl, pk, limit := "", []byte{}, 8192
	for {
		rows, err := conn.QueryContext(ctx, `SELECT tbl, pk, col, hlc, site, val FROM main._repl_clock_cells
			WHERE tbl > ? OR (tbl = ? AND pk >= ?) ORDER BY tbl, pk LIMIT ?`, tbl, tbl, pk, limit)
		if err != nil {
			return err
		}
		type key struct {
			tbl   string
			pk    []byte
			cells map[string]clockCell
		}
		var keys []key
		n := 0
		for rows.Next() {
			var k key
			var col string
			var c clockCell
			var hlc int64
			if err := rows.Scan(&k.tbl, &k.pk, &col, &hlc, &c.site, &c.val); err != nil {
				rows.Close()
				return err
			}
			c.hlc = HLC(hlc)
			if i := len(keys) - 1; i >= 0 && keys[i].tbl == k.tbl && bytes.Equal(keys[i].pk, k.pk) {
				keys[i].cells[col] = c
			} else {
				k.cells = map[string]clockCell{col: c}
				keys = append(keys, k)
			}
			n++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		full := n == limit
		if full && len(keys) == 1 {
			limit *= 2
			continue
		}
		if full {
			last := keys[len(keys)-1]
			tbl, pk, keys = last.tbl, last.pk, keys[:len(keys)-1]
		}
		for len(keys) > 0 {
			chunk := keys[:min(len(keys), recsPerStmt)]
			keys = keys[len(chunk):]
			var args []any
			for _, k := range chunk {
				args, _ = recArgs(args, nil, k.tbl, k.pk, k.cells, false)
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO main._repl_clock(tbl,pk,top,rec) VALUES `+
				strings.TrimSuffix(strings.Repeat(`(?,?,?,?),`, len(chunk)), ","), args...); err != nil {
				return err
			}
		}
		if !full {
			break
		}
	}
	if _, err := conn.ExecContext(ctx, `DROP TABLE main._repl_clock_cells`); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, `INSERT INTO main._repl_meta(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, metaLayout, strconv.Itoa(clockLayout))
	return err
}
