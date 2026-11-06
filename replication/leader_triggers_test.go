package replication

import (
	"sync/atomic"
	"testing"
)

// TestLeaderTriggersAndWithoutRowid: in Leader mode a trigger runs once, on
// the writer, and what it wrote reaches the followers as rows, which apply
// them without firing their own copy (a rowid table would gain a row per
// firing) -- from genesis, from a trigger created
// later, and after the writer moves. A WITHOUT ROWID table replicates by its
// key, a changed key included.
func TestLeaderTriggersAndWithoutRowid(t *testing.T) {
	c := newCluster(t)
	var aWrites atomic.Bool
	aWrites.Store(true)
	c.openWith("a", []Option{Leader(aWrites.Load)},
		`CREATE TABLE t(id INTEGER PRIMARY KEY, v)`,
		`CREATE TABLE log(k TEXT, v TEXT, PRIMARY KEY(k, v)) WITHOUT ROWID`,
		`CREATE TABLE audit(id INTEGER PRIMARY KEY, v)`,
		`CREATE TRIGGER ins AFTER INSERT ON t BEGIN INSERT INTO log VALUES ('ins', new.v); INSERT INTO audit(v) VALUES (new.v); END`)
	c.openWith("b", []Option{Leader(func() bool { return !aWrites.Load() })})
	const logQ = `SELECT k, v FROM log ORDER BY k, v`

	c.exec("a", `INSERT INTO t(v) VALUES ('one'), ('two')`)
	c.converge("ins|one\nins|two\n", logQ)
	c.converge("2\n", `SELECT count(*) FROM audit`)

	c.exec("a", `CREATE TRIGGER del AFTER DELETE ON t BEGIN INSERT INTO log VALUES ('del', old.v); END`,
		`DELETE FROM t WHERE v = 'one'`, `UPDATE log SET v = 'TWO' WHERE v = 'two'`)
	c.converge("del|one\nins|TWO\nins|one\n", logQ)

	aWrites.Store(false)
	c.exec("b", `INSERT INTO t(v) VALUES ('three')`, `DROP TRIGGER ins`, `INSERT INTO t(v) VALUES ('four')`)
	c.converge("del|one\nins|TWO\nins|one\nins|three\n", logQ)
	c.converge("3\n", `SELECT count(*) FROM audit`)
	c.converge("del\n", `SELECT name FROM sqlite_schema WHERE type = 'trigger'`)
}
