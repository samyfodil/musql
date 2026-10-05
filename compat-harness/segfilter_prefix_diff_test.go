package compat

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/engine"
)

// TestSegFilterPrefixMatchesOracle tests TEXT/BLOB filtering against an 8-byte
// prefix optimization, ensuring edge cases around prefix ties and zero bytes work.
func TestSegFilterPrefixMatchesOracle(t *testing.T) {
	dir := t.TempDir()
	mqPath := filepath.Join(dir, "m.musq")
	mq, err := sql.Open(driver.DriverName, mqPath)
	if err != nil {
		t.Fatal(err)
	}
	defer mq.Close()
	mq.SetMaxOpenConns(1)
	c, err := sql.Open("sqlite3", filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetMaxOpenConns(1)

	rng := rand.New(rand.NewSource(8))
	stems := []string{"", "a", "ab", "abcdefg", "abcdefgh", "abcdefghi", "user@example.com-", "zz", "\xc3\xa9t\xc3\xa9"}
	key := func() string {
		s := stems[rng.Intn(len(stems))]
		switch rng.Intn(4) {
		case 0:
			return s
		case 1:
			return s + fmt.Sprint(rng.Intn(30))
		case 2:
			return s + "\x00" + fmt.Sprint(rng.Intn(3))
		}
		return s[:rng.Intn(len(s)+1)]
	}
	for _, db := range []*sql.DB{mq, c} {
		if _, err := db.Exec(`CREATE TABLE t(tv TEXT, bv BLOB)`); err != nil {
			t.Fatal(err)
		}
	}
	type row struct{ tv string; bv []byte }
	var rows []row
	for i := 0; i < 3000; i++ {
		k := key()
		tv := k
		for j := 0; j < len(tv); j++ {
			if tv[j] == 0 {
				tv = tv[:j]
				break
			}
		}
		rows = append(rows, row{tv, []byte(k)})
	}
	for _, db := range []*sql.DB{mq, c} {
		tx, _ := db.Begin()
		st, _ := tx.Prepare(`INSERT INTO t VALUES(?, ?)`)
		for _, r := range rows {
			if _, err := st.Exec(r.tv, r.bv); err != nil {
				t.Fatal(err)
			}
		}
		st.Close()
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	// INTO SEGMENTS: 3,000 rows is under compaction's floor, so without this they
	// stay in the delta and the filter merges the log -- which never reads a
	// segment cell, and so never a prefix. A first version of this test passed
	// with the prefix's tie handling deliberately broken, for exactly that reason.
	mq.Close()
	if did, err := engine.CompactSegmentFile(mqPath); err != nil || !did {
		t.Fatalf("compacting: did=%v err=%v", did, err)
	}
	if mq, err = sql.Open(driver.DriverName, mqPath); err != nil {
		t.Fatal(err)
	}
	defer mq.Close()
	mq.SetMaxOpenConns(1)
	engine.ResetSegFilterCountersForTest()
	ops := []string{"=", "<>", "<", "<=", ">", ">="}
	for i := 0; i < 400; i++ {
		b := key()
		op := ops[rng.Intn(len(ops))]
		var q string
		var arg any
		if i%2 == 0 {
			tb := b
			for j := 0; j < len(tb); j++ {
				if tb[j] == 0 {
					tb = tb[:j]
					break
				}
			}
			q, arg = fmt.Sprintf(`SELECT count(*) FROM t WHERE tv %s ?`, op), tb
		} else {
			q, arg = fmt.Sprintf(`SELECT count(*) FROM t WHERE bv %s ?`, op), []byte(b)
		}
		var got, want int64
		if err := mq.QueryRow(q, arg).Scan(&got); err != nil {
			t.Fatal(q, err)
		}
		if err := c.QueryRow(q, arg).Scan(&want); err != nil {
			t.Fatal(q, err)
		}
		if got != want {
			t.Fatalf("%s [%q]: musql %d, C %d", q, arg, got, want)
		}
	}
	if served, _ := engine.SegFilterCountersForTest(); served == 0 {
		t.Fatal("the filter-count fast path served nothing: this compared the loop, not the prefix")
	}
}
