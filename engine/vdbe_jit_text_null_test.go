package engine

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/internal/jit"
)

// TestVMJITTextCmpNullOperand: a comparison lowered to the TEXT arm answers
// a NULL operand natively as the VDBE does -- NULL stored, no jump -- and
// leaves IS to the VDBE, over columns with NULLs, a TEXT column against an
// INTEGER one, and a scalar subquery with no row. A loop comparing a column
// with that subquery stays in native code rather than leaving it on every
// row.
func TestVMJITTextCmpNullOperand(t *testing.T) {
	lowEntryThreshold(t)
	if !vmJITEnabled {
		t.Skip("no VM JIT on this platform")
	}
	setup := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, n INTEGER, s TEXT, u, v)`}
	for k := range 600 {
		s, u, v := fmt.Sprintf("'%d'", k%7), fmt.Sprintf("'w%d'", k%5), fmt.Sprint(k%4)
		switch k % 6 {
		case 0:
			s = "NULL"
		case 1:
			u = "NULL"
		case 2:
			s, u, v = "'x'", "NULL", "NULL"
		case 3:
			u = fmt.Sprint(k % 4)
		}
		setup = append(setup, fmt.Sprintf("INSERT INTO t VALUES(%d, %d, %s, %s, %s)", k, k%7, s, u, v))
	}
	queries := []string{
		"SELECT count(*) FROM t WHERE n = (SELECT n FROM t WHERE id < 0)",
		"SELECT id FROM t WHERE u = (SELECT u FROM t WHERE id < 0) OR n = 3 ORDER BY id",
		"SELECT id, u = (SELECT u FROM t WHERE id < 0), u = v, u <> v, u IS v, u IS NOT v FROM t ORDER BY id",
		"SELECT id, s = n, s <> n, s IS n, n IS NOT s, n = s FROM t ORDER BY id",
		"SELECT id FROM t WHERE u = v ORDER BY id",
		"SELECT id FROM t WHERE NOT (u = v) ORDER BY id",
		"SELECT id FROM t WHERE u IS v ORDER BY id",
		"SELECT id FROM t WHERE s = n OR s IS NULL ORDER BY id",
		"SELECT id FROM t WHERE NOT (s <> n) ORDER BY id",
	}
	if arms := vmJITOnOff(t, setup, queries, jit.VTextCmpJump, jit.VTextCmpSet); arms < len(queries) {
		t.Fatalf("%d TEXT arms over %d queries: the test checks little", arms, len(queries))
	}

	path := filepath.Join(t.TempDir(), "t.musq")
	buildDB(t, path, setup...)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	vmJITCountCursorEnters = true
	defer func() { vmJITCountCursorEnters = false }()
	before := vmJITCursorEnters.Load()
	if _, _, err := p.QueryArgs(queries[0], nil); err != nil {
		t.Fatal(err)
	}
	if n := vmJITCursorEnters.Load() - before; n > 20 {
		t.Errorf("%d entries into native code over 600 rows: it leaves at the NULL comparison", n)
	}
}
