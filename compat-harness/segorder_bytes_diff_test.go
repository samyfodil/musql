package compat

import (
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/engine"
)

// ORDER BY LIMIT over segments, comparing order and ties against the oracle.
func TestSegOrderLimitBytesMatchesOracle(t *testing.T) {
	dir := t.TempDir()
	mqPath := filepath.Join(dir, "m.musq")
	mq, err := sql.Open(driver.DriverName, mqPath)
	if err != nil {
		t.Fatal(err)
	}
	c, err := sql.Open("sqlite3", filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetMaxOpenConns(1)
	rng := rand.New(rand.NewSource(20260928))
	stems := []string{"", "a", "abcdefgh", "abcdefghij", "user@example.com-", "zz", "\xc3\xa9"}
	for _, db := range []*sql.DB{mq, c} {
		if _, err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, tv TEXT, bv BLOB, n INTEGER, iv INTEGER, rv REAL, xv)`); err != nil {
			t.Fatal(err)
		}
	}
	type row struct {
		tv string
		bv []byte
		iv int64
		rv float64
	}
	var rows []row
	for i := 0; i < 4000; i++ {
		k := stems[rng.Intn(len(stems))] + fmt.Sprint(rng.Intn(40))
		b := []byte(k)
		if rng.Intn(5) == 0 {
			b = append(b, 0, byte(rng.Intn(3)))
		}
		iv := []int64{int64(rng.Intn(50)) - 25, -1 << 63, 1<<63 - 1, 0}[rng.Intn(4)]
		rv := []float64{float64(rng.Intn(40))/4 - 5, math.Copysign(0, -1), 0, -1e300, 1e300}[rng.Intn(5)]
		rows = append(rows, row{k, b, iv, rv})
	}
	for _, db := range []*sql.DB{mq, c} {
		tx, _ := db.Begin()
		st, _ := tx.Prepare(`INSERT INTO t(tv, bv, n, iv, rv, xv) VALUES(?, ?, ?, ?, ?, ?)`)
		for i, r := range rows {
			if _, err := st.Exec(r.tv, r.bv, i, r.iv, r.rv, r.rv); err != nil {
				t.Fatal(err)
			}
		}
		st.Close()
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	mq.Close()
	if did, err := engine.CompactSegmentFile(mqPath); err != nil || !did {
		t.Fatalf("compacting: did=%v err=%v", did, err)
	}
	if mq, err = sql.Open(driver.DriverName, mqPath); err != nil {
		t.Fatal(err)
	}
	defer mq.Close()
	mq.SetMaxOpenConns(1)
	dump := func(db *sql.DB, q string) string {
		rs, err := db.Query(q)
		if err != nil {
			return "error: " + err.Error()
		}
		defer rs.Close()
		var sb strings.Builder
		for rs.Next() {
			var id, n int64
			var k any
			rs.Scan(&id, &k, &n)
			fmt.Fprintf(&sb, "%d|%v|%d\n", id, k, n)
		}
		return sb.String()
	}
	engine.SegOrderBytesServedForTest()
	for _, col := range []string{"tv", "bv", "iv", "rv", "xv", "id"} {
		for _, dir := range []string{"", " ASC", " DESC"} {
			for _, lim := range []int{1, 7, 20, 150, 5000} {
				q := fmt.Sprintf(`SELECT id, %s, n FROM t ORDER BY %s%s LIMIT %d`, col, col, dir, lim)
				if m, o := dump(mq, q), dump(c, q); m != o {
					t.Fatalf("%s\n--- musql\n%s--- oracle\n%s", q, head(m), head(o))
				}
			}
		}
	}
	if served := engine.SegOrderBytesServedForTest(); served == 0 {
		t.Fatal("segOrderLimitBytes served nothing: this compared the general path, not the byte path")
	}
}

func head(s string) string {
	if lines := strings.SplitN(s, "\n", 12); len(lines) == 12 {
		return strings.Join(lines[:11], "\n") + "\n...\n"
	}
	return s
}
