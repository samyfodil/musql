package compat

// This file tests correlated inner-seek shapes against C SQLite.
// Queries are checked to verify the join's visit order matches the oracle.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// r32kSeekSchema mirrors buildJoinSeekTestDB (engine/vdbe_join_seek_test.go):
// an outer table with duplicate, unmatched and NULL join keys, and an inner
// table carrying an INTEGER-affinity index on its join column plus a
// NOCASE-collated one, with enough duplicate keys per value that the within-key
// visit order is observable.
func r32kSeekSchema() []string {
	setup := []string{
		`CREATE TABLE o (id INTEGER PRIMARY KEY, k INTEGER, name TEXT, tag TEXT COLLATE NOCASE)`,
		`CREATE TABLE i (id INTEGER PRIMARY KEY, fk INTEGER, tag TEXT COLLATE NOCASE, payload TEXT)`,
		`CREATE INDEX ifk ON i(fk)`,
		`CREATE INDEX itag ON i(tag)`,
	}
	outer := []struct {
		id, k int
		kNull bool
		tag   string
	}{
		{1, 0, false, "A"}, {2, 3, false, "b"}, {3, 3, false, "B"}, {4, 7, false, "c"},
		{5, 777, false, "z"}, {6, 0, true, "A"}, {7, 5, false, "d"},
	}
	var rows []string
	for _, r := range outer {
		kv := strconv.Itoa(r.k)
		if r.kNull {
			kv = "NULL"
		}
		rows = append(rows, fmt.Sprintf("(%d,%s,'o%d','%s')", r.id, kv, r.id, r.tag))
	}
	setup = append(setup, `INSERT INTO o(id,k,name,tag) VALUES`+strings.Join(rows, ","))

	tags := []string{"a", "B", "c"}
	rows = rows[:0]
	for n := 1; n <= 150; n++ {
		rows = append(rows, fmt.Sprintf("(%d,%d,'%s','payload-%d')", n, n%10, tags[n%3], n))
	}
	setup = append(setup, `INSERT INTO i(id,fk,tag,payload) VALUES`+strings.Join(rows, ","))
	return setup
}

func TestR32KJoinSeekAgreesWithOracle(t *testing.T) {
	queries := []string{
		// The shapes engine/vdbe_join_seek_test.go asserts a seek hint for.
		`SELECT o.id, i.id FROM o JOIN i ON i.fk = o.k LIMIT 12`,
		`SELECT o.id, i.id FROM o JOIN i ON o.k = i.fk LIMIT 12`,
		`SELECT o.id, i.id FROM o, i WHERE i.fk = o.k LIMIT 12`,
		`SELECT o.id, i.id FROM o JOIN i ON i.tag = o.tag LIMIT 12`,
		`SELECT o.id, i.id FROM o JOIN i ON i.fk = o.k ORDER BY i.id LIMIT 12`,
		`SELECT o.id, i.id FROM o LEFT JOIN i ON i.fk = o.k LIMIT 12`,
		`SELECT o.id, i.id FROM o JOIN i ON i.id = o.k`,
		`SELECT o.id, i.id FROM o, i WHERE i.id = o.k`,
		// The order-observing readouts: a bare inner column per group, and
		// group_concat, both of which report the whole visit order back.
		`SELECT o.id, count(*), i.payload FROM o JOIN i ON i.fk = o.k GROUP BY o.id ORDER BY 1`,
		`SELECT o.id, group_concat(i.id) FROM o JOIN i ON i.fk = o.k GROUP BY o.id ORDER BY 1`,
		`SELECT o.id, group_concat(i.id) FROM o LEFT JOIN i ON i.fk = o.k AND i.id < 60 GROUP BY o.id ORDER BY 1`,
		`SELECT i.fk, count(*), o.name FROM o JOIN i ON i.tag = o.tag GROUP BY i.fk ORDER BY 1`,
		`SELECT group_concat(i.id) FROM o JOIN i ON i.fk = o.k WHERE i.id < 40`,
		`SELECT o.id, i.id, i.payload FROM o JOIN i ON i.fk = o.k AND i.id < 100 LIMIT 9`,
		`SELECT o.id, i.id FROM o JOIN i ON i.fk = o.k WHERE i.payload LIKE 'payload-1%' LIMIT 9`,
		`SELECT o.id, i.id FROM o LEFT JOIN i ON i.tag = o.tag AND i.id < 40 LIMIT 12`,
		`SELECT a.id, b.id, c.id FROM o a JOIN i b ON b.fk = a.k JOIN i c ON c.fk = a.k AND c.id < 50 LIMIT 12`,
		`SELECT a.id, b.id, c.id FROM o a JOIN i b ON b.fk = a.k LEFT JOIN i c ON c.fk = b.fk AND c.id < 30 LIMIT 12`,
		`SELECT o.id, i.id FROM o RIGHT JOIN i ON i.fk = o.k LIMIT 12`,
		`SELECT o.id, i.id FROM o JOIN i ON i.fk = o.k ORDER BY o.id LIMIT 12`,
		`SELECT o.id, i.id FROM o JOIN i ON i.fk = o.k ORDER BY i.fk LIMIT 12`,
		`SELECT DISTINCT i.fk, o.name FROM o JOIN i ON i.fk = o.k LIMIT 9`,
	}
	stmts := append(r32kSeekSchema(), queries...)
	base := len(stmts) - len(queries)

	cgo := run(t, "cgo", stmts)
	mush := run(t, "musql", stmts)
	bad := 0
	for k, q := range queries {
		cb, _ := json.Marshal(cgo[base+k])
		mb, _ := json.Marshal(mush[base+k])
		if string(cb) == string(mb) {
			continue
		}
		bad++
		t.Errorf("DIVERGES %s\n  cgo:    %s\n  musql: %s", q, cb, mb)
	}
	if bad == 0 {
		t.Logf("R32K join-seek: %d/%d shapes agree with the oracle", len(queries), len(queries))
	}
}
