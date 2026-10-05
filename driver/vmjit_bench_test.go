package driver_test

import (
	"database/sql"
	"testing"
)

// BenchmarkVMJITQueries runs SQL shapes through database/sql against a
// .musq database -- the production path -- for an A/B of the whole-program
// JIT: run once as is and once with MUSQL_JIT=0. The first group is what
// the segment kernels already answer, where the JIT should be absent and
// neutral; the second is what they do not, where any win has to come from it.
func BenchmarkVMJITQueries(b *testing.B) {
	db := benchOpen(b, "vmjit.musq")
	benchSchema(b, db)
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	st, err := tx.Prepare("INSERT INTO t(id,sec,k,v,bid,payload) VALUES(?,?,?,?,?,?)")
	if err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= benchScanRows; i++ {
		if _, err := st.Exec(i, (i*31)%benchScanRows, i%10, (i*7919)%1_000_000, 1+(i*13)%benchScanRows, "w"); err != nil {
			b.Fatal(err)
		}
	}
	st.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	for _, q := range []struct{ name, sql string }{
		{"kernel/count-filter", "SELECT count(*) FROM t WHERE v > 500000"},
		{"kernel/sum-filter", "SELECT sum(v) FROM t WHERE v > 500000"},
		{"kernel/group-by", "SELECT k, count(*), sum(v) FROM t GROUP BY k ORDER BY k"},
		{"kernel/order-limit", "SELECT id, v FROM t ORDER BY v DESC, id DESC LIMIT 20"},
		{"vdbe/expr-filter", "SELECT count(*), sum((v*7 + sec) & 1023) FROM t WHERE ((v*7 + sec) & 1023) > ((k << 6) | 5) AND v - sec > bid"},
		{"vdbe/case-proj", "SELECT sum(CASE WHEN v % 3 = 0 THEN v ELSE sec END) FROM t WHERE k <> 3"},
		{"vdbe/arith-proj", "SELECT max(v*k - sec), min(v + sec*2 - bid) FROM t"},
		{"vdbe/point", "SELECT sec FROM t WHERE id = 4242"},
		{"vdbe/join", "SELECT count(*) FROM t JOIN t AS u ON u.id = t.bid WHERE t.sec < 2000 AND u.k > t.k"},
	} {
		b.Run(q.name, func(b *testing.B) {
			drain(b, db, q.sql) // warm the plan and page caches
			b.ResetTimer()
			for range b.N {
				drain(b, db, q.sql)
			}
		})
	}
}

func drain(b *testing.B, db *sql.DB, q string) {
	rows, err := db.Query(q)
	if err != nil {
		b.Fatal(err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		b.Fatal(err)
	}
	rows.Close()
}
