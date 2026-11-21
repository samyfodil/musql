package engine

import (
	"fmt"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/samyfodil/musql/internal/jit"
)

// TestVMJITTextIntConst: a TEXT column compared under TEXT affinity with an
// INTEGER literal, or a sum of two, reads the literal's text natively and
// answers as the VDBE alone does: either side of the compare, every
// operator, a register that holds another INTEGER at run time, a sum that
// overflows, a TEXT that only looks like the number, a NULL, in a UTF-8 and a
// UTF-16 database, and beside a BLOB, which leaves the column's read to the
// VDBE. In UTF-8 a scan stays in native code where it left at every row
// before.
func TestVMJITTextIntConst(t *testing.T) {
	lowEntryThreshold(t)
	if !vmJITEnabled || !jit.VMTextIntConst {
		t.Skip("no native INTEGER-as-TEXT compare on this platform")
	}
	texts := []string{`'8'`, `'18'`, `'9'`, `'10'`, `'80'`, `'abc'`, `''`, `'-3'`, `'8.0'`, `' 8'`, `'08'`,
		`'9223372036854775807'`, `'-9223372036854775808'`, `NULL`, `'é'`, `'17'`, `'180'`, `'1'`, `'7'`}
	table := func(texts []string) []string {
		setup := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, p TEXT, i INTEGER)`}
		for id := 1; id <= 400; id++ {
			setup = append(setup, fmt.Sprintf("INSERT INTO t VALUES(%d, %s, %d)", id, texts[id%len(texts)], id%11))
		}
		return setup
	}
	setup := table(texts)
	queries := []string{
		"SELECT id FROM t WHERE p BETWEEN 8 AND 8+10 ORDER BY id",
		"SELECT id FROM t WHERE 8 <= p AND 18 >= p ORDER BY id",
		"SELECT id, p = 18, p <> 18, p < 8, p <= 8, p > 8, p >= 8 FROM t ORDER BY id",
		"SELECT id, 18 = p, 18 <> p, 8 < p, 8 <= p, 8 > p, 8 >= p FROM t ORDER BY id",
		"SELECT id FROM t WHERE p > -3 OR p = 9223372036854775807 ORDER BY id",
		"SELECT id FROM t WHERE p < -9223372036854775807 - 1 ORDER BY id",
		"SELECT id FROM t WHERE p >= 9223372036854775807 + 1 ORDER BY id",
		"SELECT id FROM t WHERE p >= CASE WHEN i % 2 THEN 8 ELSE 9 END ORDER BY id",
		"SELECT id FROM t WHERE p >= i ORDER BY id",
		"SELECT count(*), sum(p >= 1 AND p <= 1+8) FROM t",
		"SELECT id FROM t WHERE p IS 8 OR p IS NOT 18 ORDER BY id",
		"SELECT id FROM t WHERE p >= 8 ORDER BY id",
		"SELECT id FROM t WHERE p < 18 ORDER BY id",
		"SELECT id FROM t WHERE p BETWEEN 1 AND 8 ORDER BY id",
		"SELECT id FROM t WHERE p BETWEEN 10 AND 10+7 ORDER BY id",
		"SELECT id FROM t WHERE p = 18 OR p = 7 ORDER BY id",
		"SELECT id FROM t WHERE p <> -3 ORDER BY id",
	}
	for _, enc := range []string{"UTF-8", "UTF-16le"} {
		db := append([]string{"PRAGMA encoding = '" + enc + "'"}, setup...)
		vmJITOnOff(t, db, queries, jit.VTextCmpJump, jit.VTextCmpSet)
	}
	vmJITOnOff(t, table(append(texts, `X'38'`)), queries, jit.VTextCmpJump, jit.VTextCmpSet)

	path := filepath.Join(t.TempDir(), "t.musq")
	buildDB(t, path, setup...)
	enters := func(off bool) int64 {
		old := vmIntTextOffForTest
		vmIntTextOffForTest = off
		defer func() { vmIntTextOffForTest = old }()
		p, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		CountVMJITCursorEntersForTest(true)
		defer CountVMJITCursorEntersForTest(false)
		// A collection marking during the scan has every string store leave
		// native code, which would count as the compare's exits.
		defer debug.SetGCPercent(debug.SetGCPercent(-1))
		runtime.GC()
		VMJITCursorEntersForTest()
		if _, _, err := p.QueryArgs("SELECT id FROM t WHERE p BETWEEN 8 AND 8+10 ORDER BY id", nil); err != nil {
			t.Fatal(err)
		}
		return VMJITCursorEntersForTest()
	}
	if off, on := enters(true), enters(false); off < 300 || on*4 > off {
		t.Fatalf("native entries: %d reading the literal's text, %d not: the scan still leaves native code", on, off)
	}
}

// TestVMJITTextRealConst: as TestVMJITTextIntConst, with a REAL literal,
// whose text is its rendering ("1.0", not "1"): either side, every operator,
// a register that holds an INTEGER or another REAL at run time, TEXTs that
// read as the same number but are not its text, in UTF-8 and UTF-16 and
// beside a BLOB. In UTF-8 a BETWEEN of two REALs stays in native code.
func TestVMJITTextRealConst(t *testing.T) {
	lowEntryThreshold(t)
	if !vmJITEnabled || !jit.VMTextIntConst {
		t.Skip("no native number-as-TEXT compare on this platform")
	}
	texts := []string{`'0.99'`, `'1.0'`, `'1'`, `'1.49'`, `'1.5'`, `'0.990'`, `'abc'`, `''`, `'1e0'`, `NULL`,
		`'9.0'`, `'1.4900000000000002'`, `'-0.5'`, `'1.0e+300'`, `'0.0'`, `' 1.0'`, `'é'`, `'2'`, `'10.25'`}
	table := func(texts []string) []string {
		setup := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, p TEXT, i INTEGER)`}
		for id := 1; id <= 400; id++ {
			setup = append(setup, fmt.Sprintf("INSERT INTO t VALUES(%d, %s, %d)", id, texts[id%len(texts)], id%11))
		}
		return setup
	}
	setup := table(texts)
	queries := []string{
		"SELECT id FROM t WHERE p BETWEEN 0.99 AND 1.49 ORDER BY id",
		"SELECT id FROM t WHERE 0.99 <= p AND 1.49 >= p ORDER BY id",
		"SELECT id, p = 1.0, p <> 1.0, p < 1.5, p <= 1.5, p > 1.5, p >= 1.5 FROM t ORDER BY id",
		"SELECT id, 1.0 = p, 1.0 <> p, 1.5 < p, 1.5 <= p, 1.5 > p, 1.5 >= p FROM t ORDER BY id",
		"SELECT id FROM t WHERE p = 1e300 OR p = -0.5 OR p = 0.0 ORDER BY id",
		"SELECT id FROM t WHERE p > 1.4900000000000002 ORDER BY id",
		"SELECT id FROM t WHERE p >= CASE WHEN i % 2 THEN 1.5 ELSE 1 END ORDER BY id",
		"SELECT id FROM t WHERE p >= CASE WHEN i % 3 THEN 0.99 ELSE 9.0 END ORDER BY id",
		"SELECT count(*), sum(p >= 0.99 AND p <= 10.25) FROM t",
		"SELECT id FROM t WHERE p IS 1.0 OR p IS NOT 1.5 ORDER BY id",
		"SELECT id FROM t WHERE p > 1 ORDER BY id",
		"SELECT id FROM t WHERE p > 1.0 ORDER BY id",
		"SELECT id FROM t WHERE p < 9.0 AND p > 0.5 ORDER BY id",
	}
	for _, enc := range []string{"UTF-8", "UTF-16le"} {
		db := append([]string{"PRAGMA encoding = '" + enc + "'"}, setup...)
		vmJITOnOff(t, db, queries, jit.VTextCmpJump, jit.VTextCmpSet)
	}
	vmJITOnOff(t, table(append(texts, `X'312E30'`)), queries, jit.VTextCmpJump, jit.VTextCmpSet)

	path := filepath.Join(t.TempDir(), "t.musq")
	buildDB(t, path, setup...)
	enters := func(off bool) int64 {
		old := vmIntTextOffForTest
		vmIntTextOffForTest = off
		defer func() { vmIntTextOffForTest = old }()
		p, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		CountVMJITCursorEntersForTest(true)
		defer CountVMJITCursorEntersForTest(false)
		defer debug.SetGCPercent(debug.SetGCPercent(-1))
		runtime.GC()
		VMJITCursorEntersForTest()
		if _, _, err := p.QueryArgs("SELECT id FROM t WHERE p BETWEEN 0.99 AND 1.49 ORDER BY id", nil); err != nil {
			t.Fatal(err)
		}
		return VMJITCursorEntersForTest()
	}
	if off, on := enters(true), enters(false); off < 300 || on*4 > off {
		t.Fatalf("native entries: %d reading the literals' text, %d not: the scan still leaves native code", on, off)
	}
}
