package sqlite

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
)

// TestExportEndsWhereItsCommitMovedPages is an auto_vacuum export whose
// index fill frees a page: the commit moves the last page down into it, and
// the file must end there. It ended a page later, at a page nothing
// references whose pointer-map entry named a cell that had moved, and C's
// next commit that moved pages reported the file malformed.
//
// The rows are boundedExportDB's, with w.v's blobs drawn from a fixed PCG
// stream: this one (seed 3895) frees one page while filling w_v.
func TestExportEndsWhereItsCommitMovedPages(t *testing.T) {
	const n = 6000
	r := rand.New(rand.NewPCG(3895, 7))
	stmts := []string{`PRAGMA auto_vacuum = FULL`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT UNIQUE, c REAL, d BLOB, e TEXT COLLATE NOCASE)`,
		fmt.Sprintf(`INSERT INTO t(b, c, d, e) SELECT 'b'||value||substr('xyzzy', 1, value%%5),
			value%%101*0.5, CASE WHEN value%%97=0 THEN zeroblob(9000) ELSE zeroblob(value%%40) END,
			CASE value%%3 WHEN 0 THEN 'Mixed'||value%%50 ELSE 'mixed'||value%%70 END
			FROM generate_series(1, %d)`, n),
		`DELETE FROM t WHERE a%7=0`,
		`CREATE INDEX t_c ON t(c DESC, e)`,
		`CREATE INDEX t_e ON t(e)`,
		`CREATE INDEX t_part ON t(length(d)) WHERE c > 10`,
		`CREATE TABLE w(k TEXT, n INT, v, PRIMARY KEY(n DESC, k)) WITHOUT ROWID`,
	}
	var b strings.Builder
	for value := 1; value <= n/2; value++ {
		if b.Len() == 0 {
			b.WriteString("INSERT INTO w VALUES")
		} else {
			b.WriteString(",")
		}
		blob := make([]byte, value%300)
		for i := range blob {
			blob[i] = byte(r.Uint32())
		}
		fmt.Fprintf(&b, "('k%d', %d, x'%s')", value*7919%n, value%13, hex.EncodeToString(blob))
		if value%400 == 0 {
			stmts = append(stmts, b.String())
			b.Reset()
		}
	}
	stmts = append(stmts, b.String(), `CREATE INDEX w_v ON w(v)`)
	src := filepath.Join(t.TempDir(), "x.musq")
	buildDB(t, src, stmts...)
	for _, cache := range []int{1 << 30, 8} {
		out := exportWith(t, src, 512, cache, 1)
		if pages := binary.BigEndian.Uint32(out[28:]); int(pages)*512 != len(out) {
			t.Fatalf("cache %d: the header says %d pages, the file holds %d", cache, pages, len(out)/512)
		}
		p := openBytes(t, out)
		res, err := p.checkStructuralIntegrity()
		p.Close()
		if err != nil || !res.OK {
			t.Fatalf("cache %d: %+v %v", cache, res, err)
		}
	}
}
