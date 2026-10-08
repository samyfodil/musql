package engine

import (
	"fmt"
	"strings"
	"testing"
)

// TestRecInlineMatchesTheSubProgram: a recursive step inlined into the queue
// program (recInlinePeephole) answers exactly what the step's own sub-program
// did, for every queue discipline, and a runaway recursion still stops at the
// same cap.
func TestRecInlineMatchesTheSubProgram(t *testing.T) {
	p := newSegPair(t, `CREATE TABLE t(id INTEGER PRIMARY KEY, sec INTEGER, payload TEXT)`)
	queries := []string{
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 500) SELECT count(*), sum(i), max(i) FROM c`,
		`WITH RECURSIVE c(i, s) AS (SELECT 1, 'a' UNION ALL SELECT i + 1, s || i FROM c WHERE i < 40) SELECT i, s FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c LIMIT 25) SELECT group_concat(i) FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c LIMIT 10 OFFSET 5) SELECT group_concat(i) FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION SELECT (i * 7) % 31 FROM c) SELECT count(*), sum(i) FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i * 2 FROM c WHERE i < 1000 UNION ALL SELECT i * 3 FROM c WHERE i < 1000) SELECT count(*), sum(i) FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 5 UNION ALL SELECT i - 1 FROM c WHERE i > 0 ORDER BY 1 DESC) SELECT group_concat(i) FROM c`,
		`WITH RECURSIVE c(i, f) AS (SELECT 1, 1.5 UNION ALL SELECT i + 1, f * 2 / 3 FROM c WHERE i < 30) SELECT sum(f), max(i) FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < ?) SELECT count(*) FROM c`,
		`WITH RECURSIVE c(i, n) AS (SELECT 1, NULL UNION ALL SELECT i + 1, CASE WHEN i % 3 = 0 THEN NULL ELSE i END FROM c WHERE i < 50) SELECT count(n), sum(n) FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c) SELECT count(*) FROM c`, // runaway: the cap
	}
	ask := func(q string, inline bool) string {
		recInlineOffForTest = !inline
		defer func() { recInlineOffForTest = false }()
		n, err := OpenWrite(p.path)
		if err != nil {
			t.Fatal(err)
		}
		defer n.Discard()
		var args []Value
		if strings.Contains(q, "?") {
			args = []Value{{Typ: Int, I: 77}}
		}
		_, rows, err := n.Query(q, args)
		if err != nil {
			return "error: " + err.Error()
		}
		return fmt.Sprint(typedRows(rows))
	}
	for _, q := range queries {
		if want, got := ask(q, false), ask(q, true); got != want {
			t.Errorf("%s:\n inlined     %s\n sub-program %s", q, got, want)
		}
	}
	// The recursive INSERT the browser race loads its table with.
	load := func(inline bool) string {
		recInlineOffForTest = !inline
		defer func() { recInlineOffForTest = false }()
		n, err := OpenWrite(p.path)
		if err != nil {
			t.Fatal(err)
		}
		defer n.Discard()
		if err := n.Exec(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 2000) INSERT INTO t SELECT i, (i * 7919) % 1000, 'row-' || i || '-payload' FROM c`); err != nil {
			t.Fatal(err)
		}
		_, rows, err := n.Query(`SELECT count(*), sum(id), sum(sec), max(payload), min(payload) FROM t`, nil)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(typedRows(rows))
	}
	if want, got := load(false), load(true); got != want {
		t.Errorf("recursive INSERT:\n inlined     %s\n sub-program %s", got, want)
	}
}
