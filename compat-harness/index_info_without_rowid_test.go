// This file gates PRAGMA index_info / index_xinfo for WITHOUT ROWID tables.
// In such tables, the PRIMARY KEY is the index structure and secondary indexes
// carry the full primary key to identify rows.
package compat

import "testing"

var iwrSetup = []string{
	`CREATE TABLE wr(k TEXT PRIMARY KEY, v, w) WITHOUT ROWID`,
	`CREATE INDEX wrv ON wr(v)`,
	`CREATE UNIQUE INDEX wrw ON wr(w)`,
	// a COMPOSITE primary key, so PK ORDER is distinguishable from table order
	`CREATE TABLE wr2(k INT, j INT, v, PRIMARY KEY(k,j)) WITHOUT ROWID`,
	`CREATE INDEX wr2v ON wr2(v)`,
	// an index that ALREADY covers a PK column: it must not be repeated
	`CREATE INDEX wr2vk ON wr2(v,k)`,
	`CREATE INDEX wr2all ON wr2(k,j,v)`,
	// a collated PK, which the trailing rows must report as the column's own
	`CREATE TABLE wr3(k TEXT COLLATE NOCASE PRIMARY KEY, v) WITHOUT ROWID`,
	// ...and the ROWID controls
	`CREATE TABLE rt(a INTEGER PRIMARY KEY, b UNIQUE, c)`,
	`CREATE INDEX rtc ON rt(c)`,
	`CREATE TABLE rt2(a, b, PRIMARY KEY(a,b))`,
}

func TestIndexInfoWithoutRowid(t *testing.T) {
	for _, q := range []string{
		// the PK index reached through the TABLE name
		`PRAGMA index_info(wr)`, `PRAGMA index_xinfo(wr)`,
		`PRAGMA index_info(wr2)`, `PRAGMA index_xinfo(wr2)`,
		`PRAGMA index_info(wr3)`, `PRAGMA index_xinfo(wr3)`,
		// secondary indexes on a WITHOUT ROWID table
		`PRAGMA index_info(wrv)`, `PRAGMA index_xinfo(wrv)`,
		`PRAGMA index_info(wrw)`, `PRAGMA index_xinfo(wrw)`,
		`PRAGMA index_info(wr2v)`, `PRAGMA index_xinfo(wr2v)`,
		`PRAGMA index_info(wr2vk)`, `PRAGMA index_xinfo(wr2vk)`,
		`PRAGMA index_info(wr2all)`, `PRAGMA index_xinfo(wr2all)`,
		// ROWID controls: the table name answers NOTHING, and an ordinary
		// index still gets the rowid pseudo-column
		`PRAGMA index_info(rt)`, `PRAGMA index_xinfo(rt)`,
		`PRAGMA index_info(rt2)`, `PRAGMA index_xinfo(rt2)`,
		`PRAGMA index_info(rtc)`, `PRAGMA index_xinfo(rtc)`,
		`PRAGMA index_info(sqlite_autoindex_rt_1)`,
		`PRAGMA index_xinfo(sqlite_autoindex_rt_1)`,
		`PRAGMA index_info(sqlite_autoindex_rt2_1)`,
		`PRAGMA index_xinfo(sqlite_autoindex_rt2_1)`,
		// a name that is neither
		`PRAGMA index_info(nope)`, `PRAGMA index_xinfo(nope)`,
		// index_list, which must be unaffected either way
		`PRAGMA index_list(wr)`, `PRAGMA index_list(wr2)`,
		`PRAGMA index_list(wr3)`, `PRAGMA index_list(rt)`,
		// the quoted and assignment spellings of the same argument
		`PRAGMA index_xinfo('wr')`, `PRAGMA index_xinfo("wr")`,
		`PRAGMA index_xinfo=wr`, `PRAGMA index_info('wr2')`,
		// ...and through the schema-qualified form
		`PRAGMA main.index_xinfo(wr)`, `PRAGMA main.index_info(wr2)`,
	} {
		prDifferQ(t, q, iwrSetup, q)
	}
}

// TestIndexInfoWithoutRowidTemp runs the same lookup in the TEMP catalog,
// which resolves through a different scope.
func TestIndexInfoWithoutRowidTemp(t *testing.T) {
	setup := []string{
		`CREATE TEMP TABLE twr(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
		`CREATE TEMP TABLE trt(a INTEGER PRIMARY KEY, b)`,
	}
	for _, q := range []string{
		`PRAGMA index_info(twr)`, `PRAGMA index_xinfo(twr)`,
		`PRAGMA index_info(trt)`, `PRAGMA index_xinfo(trt)`,
		`PRAGMA temp.index_info(twr)`, `PRAGMA temp.index_xinfo(twr)`,
	} {
		prDifferQ(t, q, setup, q)
	}
}
