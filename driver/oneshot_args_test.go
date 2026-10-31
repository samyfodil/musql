package driver

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestOneShotExecArgumentArena: an unprepared db.Exec copies its TEXT and BLOB
// arguments into an arena the size they take, not into a chunk sized for a
// statement run many times, and every value lands as given.
func TestOneShotExecArgumentArena(t *testing.T) {
	db, err := sql.Open(DriverName, filepath.Join(t.TempDir(), "a.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t(a, b, c, d, e)`); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", argArenaMax+1)
	if _, err := db.Exec(`INSERT INTO t VALUES(?, ?, ?, ?, ?)`, "", "abc", long, []byte{1, 2}, []byte("de")); err != nil {
		t.Fatal(err)
	}
	var a, b, c string
	var d, e []byte
	var ta string
	if err := db.QueryRow(`SELECT a, b, c, d, e, typeof(a) FROM t`).Scan(&a, &b, &c, &d, &e, &ta); err != nil {
		t.Fatal(err)
	}
	if a != "" || ta != "text" || b != "abc" || c != long || !bytes.Equal(d, []byte{1, 2}) || string(e) != "de" {
		t.Fatalf("got %q (%s) %q %d bytes %v %q", a, ta, b, len(c), d, e)
	}

	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(t.Context(), `BEGIN`); err != nil {
		t.Fatal(err)
	}
	const n = 200
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range n {
		if _, err := conn.ExecContext(t.Context(), `INSERT INTO t(b) VALUES(?)`, "abc"); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	if _, err := conn.ExecContext(t.Context(), `COMMIT`); err != nil {
		t.Fatal(err)
	}
	if per := (after.TotalAlloc - before.TotalAlloc) / n; per >= argArenaChunk {
		t.Fatalf("%d bytes allocated per one-shot INSERT of a 3-byte argument", per)
	}
}

// TestConnExecKeepsParamInfo: Conn.Exec and Conn.Query, which a capture hook
// runs for every row it writes, keep a statement's parameter info as a prepare
// does instead of lexing its text again on every call.
func TestConnExecKeepsParamInfo(t *testing.T) {
	db, err := sql.Open(DriverName, filepath.Join(t.TempDir(), "p.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t(a, b)`); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	const ins, sel = `INSERT INTO t VALUES(?, ?)`, `SELECT b FROM t WHERE a = ?`
	if err := conn.Raw(func(dc any) error {
		c := dc.(*Conn)
		if err := c.Exec(t.Context(), ins, 1, "x"); err != nil {
			return err
		}
		rows, err := c.Query(t.Context(), sel, 1)
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0][0] != "x" {
			t.Errorf("got %v", rows)
		}
		for _, q := range []string{ins, sel} {
			if _, ok := c.paramInfo[q]; !ok {
				t.Errorf("%s: no parameter info kept", q)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
