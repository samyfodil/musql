package compat

// Tests character-indexed scalar functions over malformed UTF-8, where
// SQLite's decoder differs from Go's utf8 package.

import (
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// TestMalformedUTF8ReaderMatrix verifies that character boundaries and decoded
// values are computed correctly for all lead-byte classes and continuation patterns.
func TestMalformedUTF8ReaderMatrix(t *testing.T) {
	leads := []string{"41", "7f", "80", "9f", "bf", "c0", "c1", "c2", "df",
		"e0", "ed", "ef", "f0", "f4", "f7", "f8", "fb", "fc", "fd", "fe", "ff"}
	tails := []string{"", "41", "80", "bf", "c2", "8080", "80bf80", "808080"}
	for _, l := range leads {
		for _, tl := range tails {
			h := l + tl
			v := fmt.Sprintf("cast(x'%s' as text)", h)
			q := fmt.Sprintf(
				"SELECT length(%s), unicode(%s), hex(substr(%s,1,1)), hex(substr(%s,2,1)), hex(substr(%s,-1))",
				v, v, v, v, v)
			if !differ(t, "utf8matrix/"+h, []string{q}) {
				t.Errorf("diverged on: %s", q)
			}
		}
	}
}

// TestMalformedUTF8Builtins tests character-based functions that must handle
// malformed UTF-8 correctly, including length(), instr(), substr(), trim(), and quote().
func TestMalformedUTF8Builtins(t *testing.T) {
	vals := []string{
		"cast(x'41c3a942' as text)", // A é B  -- well formed, the control
		"cast(x'41ff42' as text)",   // A <ff> B
		"cast(x'e28241' as text)",   // truncated 3-byte sequence, then A
		"cast(x'f09f9280' as text)", // a complete 4-byte sequence
		"cast(x'c3a9c3a9' as text)", // é é
		"cast(x'00' as text)",       // the embedded-NUL boundary, still gated
		"cast(x'410042' as text)",
	}
	for _, v := range vals {
		for _, q := range []string{
			fmt.Sprintf("SELECT length(%s), unicode(%s), octet_length(%s), typeof(%s)", v, v, v, v),
			fmt.Sprintf("SELECT hex(substr(%s,1,1)), hex(substr(%s,2,1)), hex(substr(%s,3,1)), hex(substr(%s,2)), hex(substr(%s,-2)), hex(substr(%s,-2,1))", v, v, v, v, v, v),
			fmt.Sprintf("SELECT instr(%s,'A'), instr(%s,'B'), instr(%s,cast(x'ff' as text)), instr(%s,cast(x'c3a9' as text))", v, v, v, v),
			fmt.Sprintf("SELECT hex(trim(%s,cast(x'ff' as text))), hex(ltrim(%s,'A')), hex(rtrim(%s,'B')), hex(trim(%s,cast(x'c3a9' as text))), hex(trim(%s))", v, v, v, v, v),
			fmt.Sprintf("SELECT hex(quote(%s)), hex(replace(%s,cast(x'ff' as text),'Z')), hex(%s||%s)", v, v, v, v),
			fmt.Sprintf("SELECT hex(upper(%s)), hex(lower(%s)), hex(printf('%%s',%s)), hex(printf('%%q',%s)), hex(printf('%%Q',%s))", v, v, v, v, v),
			fmt.Sprintf("SELECT %s LIKE 'A%%', %s GLOB 'A*', %s GLOB 'A?B', %s LIKE 'A_B'", v, v, v, v),
			fmt.Sprintf("SELECT hex(json_quote(%s)), json_valid(%s)", v, v),
		} {
			if !differ(t, "utf8builtin", []string{q}) {
				t.Errorf("diverged on: %s", q)
			}
		}
	}
}

// TestMalformedUTF8Stored runs the same values through a real table, so the
// record encoder/decoder, the comparison rules, GROUP BY, ORDER BY and an index
// seek all see them too -- the byte-exactness has to survive storage, not just
// expression evaluation.
func TestMalformedUTF8Stored(t *testing.T) {
	stmts := []string{
		"CREATE TABLE t(x TEXT)",
		"INSERT INTO t VALUES(cast(x'00' as text)),(cast(x'4100' as text))," +
			"(cast(x'41' as text)),(cast(x'ff' as text)),(cast(x'c3a9' as text))," +
			"(cast(x'41ff42' as text)),(cast(x'e282' as text))",
		"SELECT hex(x), length(x), octet_length(x), unicode(x), typeof(x) FROM t ORDER BY rowid",
		"SELECT hex(x), count(*) FROM t GROUP BY x ORDER BY hex(x)",
		"SELECT hex(x) FROM t ORDER BY x",
		"SELECT hex(x) FROM t WHERE x = cast(x'41' as text)",
		"SELECT hex(x) FROM t WHERE x LIKE 'A%'",
		"SELECT hex(x) FROM t WHERE x GLOB 'A*'",
		"SELECT hex(min(x)), hex(max(x)), hex(group_concat(hex(x),'|')) FROM t",
		"CREATE INDEX tx ON t(x)",
		"SELECT hex(x) FROM t WHERE x > cast(x'41' as text) ORDER BY x",
	}
	if !differ(t, "utf8stored", stmts) {
		t.Error("diverged storing malformed UTF-8")
	}
}
