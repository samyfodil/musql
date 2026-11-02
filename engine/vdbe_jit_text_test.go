package engine

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/internal/jit"
)

// TestVMJITTextQueries: substr and TEXT = and <> answer with the VM JIT as
// the VDBE alone answers them, in a UTF-8 and a UTF-16 database, over the
// TEXTs native code takes and the ones it leaves to the VDBE: a position past
// the end or before the start, a NUL or a non-ASCII byte before the
// character, a BLOB, a NULL, a REAL or TEXT position, a mixed comparison.
func TestVMJITTextQueries(t *testing.T) {
	lowEntryThreshold(t)
	if !vmJITEnabled {
		t.Skip("no VM JIT on this platform")
	}
	texts := []string{`'hello'`, `''`, `'h'`, `'héllo'`, `'é'`, `CAST(x'6162006364' AS TEXT)`, `CAST(x'41ff42' AS TEXT)`,
		`'abcdefghijklmnopq'`, `'abcdefgh'`, `'53..7....6..195....98....6.8...6...34..8.3..17...2...6.6....28....419..5....8..79'`,
		`NULL`, `7`, `X'6162'`, `2.5`}
	chars := []string{`'h'`, `'e'`, `'.'`, `'5'`, `''`, `'é'`, `NULL`, `5`, `'hello'`}
	// u is s with no affinity, and v a character with none: their
	// comparisons are under no affinity, c's and s's under TEXT affinity.
	setup := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, s TEXT, c TEXT, i INTEGER, r, u, v)`}
	id := 0
	for k, s := range texts {
		for j := range 4 {
			id++
			c := chars[(k+j)%len(chars)]
			setup = append(setup, fmt.Sprintf("INSERT INTO t VALUES(%d, %s, %s, %d, %s, %s, %s)",
				id, s, c, (k*3+j*5)%14-2, []string{"1", "2.0", "'3'", "NULL"}[j], s, c))
		}
	}
	queries := []string{
		"SELECT id, substr(s, i, 1) FROM t ORDER BY id",
		"SELECT id, substr(s, r, 1), substr(s, i, 2), substr(s, i, r), substring(s, i, 1) FROM t ORDER BY id",
		"SELECT id, s = c, s <> c, c = substr(s, i, 1), substr(s, i, 1) <> c FROM t ORDER BY id",
		"SELECT id FROM t WHERE s = c ORDER BY id",
		"SELECT id FROM t WHERE s <> c ORDER BY id",
		"SELECT id FROM t WHERE substr(s, i, 1) = c ORDER BY id",
		"SELECT id FROM t WHERE c = substr(s, i, 1) OR c = substr(s, i + 1, 1) ORDER BY id",
		"SELECT id FROM t WHERE s = 'hello' OR s = '' ORDER BY id",
		"SELECT id, s = i, c = 5, c IS s, c IS NOT s FROM t ORDER BY id",
		"SELECT id FROM t WHERE c = s COLLATE NOCASE ORDER BY id",
		"SELECT id, u = v, u <> v, v = substr(u, i, 1), substr(u, i, 1) <> v, u = 'hello', r = 1, r = '3', r <> 2.0, u = r FROM t ORDER BY id",
		"SELECT id FROM t WHERE u = v OR v = substr(u, i, 1) ORDER BY id",
		"SELECT id FROM t WHERE u <> 'hello' AND r = 1 ORDER BY id",
		"SELECT id FROM t WHERE i + 0 = 3 OR i * 2 <> r ORDER BY id",
		"WITH c(x) AS (VALUES('a'), (5), ('5'), (NULL), (5.0), (X'35')) SELECT x, x = 5, x = '5', x <> 'a' FROM c",
		`WITH RECURSIVE
  input(sud) AS (VALUES('53..7....6..195....98....6.8...6...34..8.3..17...2...6.6....28....419..5....8..79')),
  digits(z, lp) AS (VALUES('1', 1) UNION ALL SELECT CAST(lp+1 AS TEXT), lp+1 FROM digits WHERE lp<9),
  x(s, ind) AS (
    SELECT sud, instr(sud, '.') FROM input
    UNION ALL
    SELECT substr(s, 1, ind-1) || z || substr(s, ind+1), instr( substr(s, 1, ind-1) || z || substr(s, ind+1), '.' )
     FROM x, digits AS z
    WHERE ind>0 AND NOT EXISTS (SELECT 1 FROM digits AS lp
             WHERE z.z = substr(s, ((ind-1)/9)*9 + lp, 1)
                OR z.z = substr(s, ((ind-1)%9) + (lp-1)*9 + 1, 1)
                OR z.z = substr(s, (((ind-1)/3) % 3) * 3 + ((ind-1)/27) * 27 + lp + ((lp-1) / 3) * 6, 1)))
SELECT s FROM x WHERE ind=0`,
	}
	for k := -1; k <= 18; k++ {
		queries = append(queries, fmt.Sprintf("SELECT id, substr(s, %d, 1) FROM t ORDER BY id", k))
	}
	queries = append(queries, "SELECT id, substr(s, 81, 1), substr(s, 82, 1) FROM t ORDER BY id")
	run := func(enc string, jitOn bool) ([]string, int) {
		old := vmJITEnabled
		vmJITEnabled = jitOn
		defer func() { vmJITEnabled = old }()
		path := filepath.Join(t.TempDir(), "t.musq")
		buildDB(t, path, append([]string{"PRAGMA encoding = '" + enc + "'"}, setup...)...)
		p, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		out := make([]string, len(queries))
		arms := 0
		for i, q := range queries {
			cols, rows, err := p.QueryArgs(q, nil)
			if err != nil {
				out[i] = "error: " + err.Error()
				continue
			}
			var b strings.Builder
			fmt.Fprint(&b, cols)
			for _, row := range rows {
				for _, v := range row {
					fmt.Fprintf(&b, " %d:%d:%x:%q", v.Typ, v.I, math.Float64bits(v.F), v.S)
				}
				b.WriteString(";")
			}
			out[i] = b.String()
			if stmt, err := ParseSelect(q); err == nil {
				if prog, err := compileSelectScan(p, stmt, nil); err == nil {
					for k := range prog.Insns {
						switch lowerVM(&prog.Insns[k], prog.NReg).Op {
						case jit.VSubstr, jit.VTextCmpJump, jit.VTextCmpSet:
							arms++
						}
					}
				}
			}
		}
		return out, arms
	}
	for _, enc := range []string{"UTF-8", "UTF-16le"} {
		want, _ := run(enc, false)
		got, arms := run(enc, true)
		for i := range queries {
			if want[i] != got[i] {
				t.Fatalf("%s: JIT diverges on %s\nvdbe %.600s\njit  %.600s", enc, queries[i], want[i], got[i])
			}
			if strings.HasPrefix(got[i], "error: ") {
				t.Fatalf("%s: %s: %s", enc, queries[i], got[i])
			}
		}
		if arms < len(queries) {
			t.Fatalf("%s: %d TEXT arms over %d queries: the test checks little", enc, arms, len(queries))
		}
		t.Logf("%s: %d queries agree; %d TEXT arms", enc, len(queries), arms)
	}
}
