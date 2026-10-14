package compat

// UPDATE ... FROM where one target row matches several FROM rows. C's pass one
// writes the join's rows into an ephemeral table keyed by the target's rowid
// with OP_Insert, which overwrites, so the last row in its plan order wins.
// Where the ported planner decides that order the engine keeps the same last
// row; elsewhere it still declines (a FROM whose joins carry ON or USING puts
// the target after them here, which is not C's order, so "FROM s JOIN u ON
// ..." is not among these). Each case runs the UPDATE and reads the target
// back.

import (
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func TestUpdateFromLastMatch(t *testing.T) {
	targets := map[string]string{
		"ipk":   "CREATE TABLE t(id INTEGER PRIMARY KEY, v)",
		"intpk": "CREATE TABLE t(id INT PRIMARY KEY, v, k INT)",
		"plain": "CREATE TABLE t(id INT, v)",
	}
	sources := map[string][]string{
		"noidx":   {"CREATE TABLE s(id INT, x INT, w TEXT)"},
		"idx_id":  {"CREATE TABLE s(id INT, x INT, w TEXT)", "CREATE INDEX s_id ON s(id)"},
		"idx_idx": {"CREATE TABLE s(id INT, x INT, w TEXT)", "CREATE INDEX s_idx ON s(id, x DESC)"},
		"idx_x":   {"CREATE TABLE s(id INT, x INT, w TEXT)", "CREATE INDEX s_x ON s(x)"},
	}
	data := []string{
		"INSERT INTO s VALUES(1,5,'e'),(1,2,'b'),(2,9,'z'),(1,7,'g'),(2,1,'a'),(3,3,'c')",
		"CREATE TABLE u(id INT, y TEXT)",
		"INSERT INTO u VALUES(1,'p'),(1,'q'),(2,'r')",
	}
	updates := []string{
		"UPDATE t SET v = s.w FROM s WHERE t.id = s.id",
		"UPDATE t SET v = s.w FROM s WHERE s.id = t.id AND s.x > 1",
		"UPDATE t SET v = s.w || s.x FROM s WHERE t.id = s.id AND s.w <> 'zz'",
		"UPDATE t SET v = s.w || u.y FROM s, u WHERE t.id = s.id AND u.id = s.id",
		"UPDATE t SET v = s.w FROM s WHERE t.id = s.id AND s.x BETWEEN 2 AND 7",
		"UPDATE t NOT INDEXED SET v = s.w FROM s WHERE t.id = s.id",
		"UPDATE t SET v = s.w || u.y FROM s, u WHERE t.id = s.id AND u.id = t.id",
		"UPDATE t SET v = s.w FROM s WHERE t.id + 0 = s.id",
		"UPDATE t SET v = s.w FROM s WHERE t.id IN (1, 2) AND s.id = t.id",
		"UPDATE t SET v = s.w FROM s WHERE s.id = t.id AND (s.x = 5 OR s.x = 2 OR s.w = 'a')",
		"UPDATE t SET v = s.w FROM s WHERE s.id >= t.id",
	}
	for tn, tsql := range targets {
		for sn, ssql := range sources {
			for ui, u := range updates {
				name := fmt.Sprintf("%s/%s/%d", tn, sn, ui)
				t.Run(name, func(t *testing.T) {
					stmts := []string{tsql}
					if tn == "intpk" {
						stmts = append(stmts, "CREATE INDEX t_k ON t(k)", "INSERT INTO t VALUES(1,'o1',10),(2,'o2',20),(3,'o3',30)")
					} else {
						stmts = append(stmts, "INSERT INTO t(id, v) VALUES(1,'o1'),(2,'o2'),(3,'o3')")
					}
					stmts = append(stmts, ssql...)
					stmts = append(stmts, data...)
					stmts = append(stmts, u, "SELECT rowid, * FROM t ORDER BY rowid")
					differ(t, name, stmts)
				})
			}
		}
	}
}
