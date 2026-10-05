package engine

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"
)

// TestSerialSkipLenMatches verifies skipped column validation matches decoded validation.
func TestSerialSkipLenMatches(t *testing.T) {
	body := make([]byte, 64)
	for i := range body {
		body[i] = byte(i + 1)
	}
	for st := uint64(0); st <= 160; st++ {
		for n := 0; n <= len(body); n++ {
			b := body[:n]
			var v Value
			wantSize, wantErr := decodeSerialInto(st, b, &v)
			gotSize, gotErr := serialSkipLen(st, b)
			if gotSize != wantSize {
				t.Fatalf("serial %d, %d body bytes: skip size %d, decode size %d", st, n, gotSize, wantSize)
			}
			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("serial %d, %d body bytes: skip err %v, decode err %v", st, n, gotErr, wantErr)
			}
			if gotErr != nil && gotErr.Error() != wantErr.Error() {
				t.Fatalf("serial %d, %d body bytes: skip err %q, decode err %q", st, n, gotErr, wantErr)
			}
		}
	}
}

// maskTestRow creates a test row with one column of each serial type.
func maskTestRow() []Value {
	return []Value{
		{Typ: Null},                                      // 0: serial 0
		{Typ: Int, I: 0},                                 // 1: serial 8 (constant, no body)
		{Typ: Int, I: 1},                                 // 2: serial 9 (constant, no body)
		{Typ: Int, I: -7},                                // 3: serial 1
		{Typ: Int, I: 300},                               // 4: serial 2
		{Typ: Int, I: 1 << 20},                           // 5: serial 3
		{Typ: Int, I: 1 << 28},                           // 6: serial 4
		{Typ: Int, I: 1 << 44},                           // 7: serial 5
		{Typ: Int, I: math.MinInt64},                     // 8: serial 6
		{Typ: Float, F: -1.5},                            // 9: serial 7
		{Typ: Text, S: []byte("")},                       // 10: serial 13, empty
		{Typ: Text, S: []byte("hello")},                  // 11: one-byte serial
		{Typ: Blob, S: []byte{}},                         // 12: serial 12, empty
		{Typ: Blob, S: []byte{0, 1, 2, 0xff}},            // 13: one-byte serial
		{Typ: Text, S: bytes.Repeat([]byte("x"), 57)},    // 14: serial 127, last one-byte
		{Typ: Text, S: bytes.Repeat([]byte("y"), 58)},    // 15: serial 129, first two-byte
		{Typ: Blob, S: bytes.Repeat([]byte{0xab}, 4000)}, // 16: a wide two-byte serial
	}
}

// TestDecodeRecordMaskedEqualsFull is the exhaustive correctness sweep: over a
// record holding one column of every serial type, EVERY column is read under
// EVERY single-column mask, under masks that want two columns, and under the
// mask that wants everything -- and every wanted column must decode to exactly
// what an unmasked decode produced for it, while every unwanted one is left as
// the zero Value (which is why nothing may read those slots directly; see
// vdbeCursor.rowVals).
func TestDecodeRecordMaskedEqualsFull(t *testing.T) {
	row := maskTestRow()
	rec := encodeRecord(row)
	full, err := decodeRecordInto(rec, nil)
	if err != nil {
		t.Fatalf("full decode: %v", err)
	}
	if len(full) != len(row) {
		t.Fatalf("full decode produced %d columns, want %d", len(full), len(row))
	}

	check := func(mask columnMask, name string) {
		t.Helper()
		got, err := decodeRecordMaskedInto(rec, nil, mask)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != len(full) {
			t.Fatalf("%s: %d columns, want %d", name, len(got), len(full))
		}
		for i := range got {
			if mask.has(i) {
				if !valuesEqualExact(got[i], full[i]) {
					t.Fatalf("%s: column %d = %v, want %v", name, i, got[i], full[i])
				}
			} else if got[i].Typ != Null || got[i].S != nil || got[i].I != 0 || got[i].F != 0 {
				t.Fatalf("%s: skipped column %d is %v, want the zero Value", name, i, got[i])
			}
		}
	}

	check(allColumns, "allColumns")
	for i := range row {
		check(1<<uint(i), fmt.Sprintf("only col %d", i))
		for j := range row {
			check(1<<uint(i)|1<<uint(j), fmt.Sprintf("cols %d+%d", i, j))
		}
	}
	// The mask that wants nothing at all: every column skipped, and the record
	// still fully validated (a scan whose program reads no column but does
	// count rows is exactly this shape).
	check(0, "empty mask")
}

// TestColumnMaskBit63 pins the "bit 63 means column 63 and everything above
// it" convention this engine borrows from SQLite (where.c:7384's
// `colUsed |= ((u64)1)<<(ii<63 ? ii : 63)`), over a record wide enough to
// have columns on both sides of the boundary.
func TestColumnMaskBit63(t *testing.T) {
	row := make([]Value, 70)
	for i := range row {
		row[i] = Value{Typ: Int, I: int64(i + 1)}
	}
	rec := encodeRecord(row)

	// Bit 63 set: every column from 63 up must decode.
	got, err := decodeRecordMaskedInto(rec, nil, 1<<63)
	if err != nil {
		t.Fatal(err)
	}
	for i := range got {
		want := Value{}
		if i >= 63 {
			want = row[i]
		}
		if !valuesEqualExact(got[i], want) {
			t.Fatalf("bit63 mask: column %d = %v, want %v", i, got[i], want)
		}
	}
	// Bit 63 clear: no column from 63 up may decode, and one below it does.
	got, err = decodeRecordMaskedInto(rec, nil, 1<<5)
	if err != nil {
		t.Fatal(err)
	}
	for i := range got {
		want := Value{}
		if i == 5 {
			want = row[5]
		}
		if !valuesEqualExact(got[i], want) {
			t.Fatalf("bit5 mask: column %d = %v, want %v", i, got[i], want)
		}
	}
	if !allColumns.has(70) || !allColumns.has(1) {
		t.Fatal("allColumns must want every column")
	}
	if columnMask(1 << 63).has(62) {
		t.Fatal("bit 63 must not imply column 62")
	}
}

// TestWidenIfTotal pins the "SELECT * takes the unmasked path" collapse.
func TestWidenIfTotal(t *testing.T) {
	if got := columnMask(0b111).widenIfTotal(3); got != allColumns {
		t.Fatalf("a mask wanting all 3 columns should widen to allColumns, got %#x", uint64(got))
	}
	if got := columnMask(0b101).widenIfTotal(3); got != 0b101 {
		t.Fatalf("a partial mask must not widen, got %#x", uint64(got))
	}
	if got := columnMask(0b111).widenIfTotal(4); got != 0b111 {
		t.Fatalf("a mask short of the column count must not widen, got %#x", uint64(got))
	}
	// A table wider than the single-word convention can express is never
	// collapsed -- bit 63 stands for a range, so "all bits set" cannot be
	// concluded from it.
	if got := allColumns.widenIfTotal(200); got != allColumns {
		t.Fatalf("allColumns must stay allColumns, got %#x", uint64(got))
	}
	if got := columnMask(3).widenIfTotal(-1); got != 3 {
		t.Fatalf("a negative column count must not widen, got %#x", uint64(got))
	}
}

// hostileRecord names one deliberately malformed record. Every one of these
// must produce an ERROR from every mask -- never a panic, never a short row,
// and never a silently-wrong one. The masked path is the one under test: an
// earlier prototype of this optimization skipped a column with a bare
// `off += serialTypeLen(st)` and no validation, which turned cases 2 and 5
// below into an out-of-range slice on the FOLLOWING column.
func hostileRecords(t *testing.T) []struct {
	name string
	rec  []byte
} {
	t.Helper()
	good := encodeRecord([]Value{
		{Typ: Int, I: 5},
		{Typ: Text, S: []byte("abcdefghij")}, // serial 33, 10 body bytes
		{Typ: Int, I: 9},
	})
	// good's header is: hdrLen=4, then serials 1, 33, 1.
	if len(good) < 6 || good[0] != 4 {
		t.Fatalf("fixture record is not the shape this test assumes: %x", good)
	}

	truncHdr := append([]byte(nil), good...)
	truncHdr = truncHdr[:2] // header declares 4 bytes but only 2 are present

	oversizeText := append([]byte(nil), good...)
	oversizeText[2] = 13 + 2*100 // column 1 now claims 100 body bytes; wait, that is > 127

	// A serial type claiming far more bytes than the record holds, written as
	// a real two-byte varint so the header stays well-formed.
	claimTooMuch := []byte{
		5,          // header length 5
		1,          // col 0: 1-byte int
		0x82, 0x21, // col 1: varint 0x121 = 289 -> TEXT of (289-13)/2 = 138 bytes
		1, // col 2: 1-byte int
		7, // one body byte for col 0
	}

	nonTerminating := []byte{
		4,                // header length 4
		1,                // col 0
		0xff, 0xff, 0xff, // a varint whose continuation bits never stop inside the record
		7,
	}

	hdrTooBig := append([]byte(nil), good...)
	hdrTooBig[0] = byte(len(good) + 40) // header length beyond the record

	// A column COUNT larger than any page could hold: a header claiming
	// thousands of one-byte serial types, with no body behind them.
	manyCols := append([]byte{}, 0x84, 0x48) // varint 0x248 = 584-byte header
	for len(manyCols) < 584 {
		manyCols = append(manyCols, 1) // a 1-byte integer column, each needing a body byte
	}
	manyCols = append(manyCols, 0, 0, 0) // only three body bytes for ~582 columns

	reserved := []byte{
		3,    // header length 3
		1,    // col 0: 1-byte int
		10,   // col 1: RESERVED serial type 10
		7, 7, // body
	}

	// A serial-type varint that STRADDLES the declared end of the header: a
	// non-minimal 3-byte encoding of serial type 9 (integer constant 1, no body
	// bytes) beginning one byte before a 3-byte header ends. Every individual
	// varint parses, the record decodes to a plausible one-column row, and the
	// only thing wrong with it is that the header walk finished PAST hdrLen --
	// which is exactly what `pos != int(hdrLen)` is there to catch, and what
	// OP_Column rejects with `zHdr>zEndHdr` (vdbe.c:3142).
	straddle := []byte{0x03, 0x80, 0x80, 0x09}

	return []struct {
		name string
		rec  []byte
	}{
		{"empty payload", nil},
		{"serial-type varint straddling the header end", straddle},
		{"truncated header", truncHdr},
		{"oversize text serial", oversizeText},
		{"serial claims more bytes than the record holds", claimTooMuch},
		{"non-terminating serial-type varint", nonTerminating},
		{"header length larger than the record", hdrTooBig},
		{"column count larger than a page", manyCols},
		{"reserved serial type 10", reserved},
		{"header length varint alone", []byte{0x81}},
	}
}

// TestDecodeRecordMaskedHostileInput runs the malformed-record battery through
// every mask shape, including masks that skip precisely the malformed column,
// asserting an error (never a panic) every time. A record that the unmasked
// decode rejects must be rejected by every masked decode too, since a masked
// row that "succeeded" would be handed to a query as real data.
func TestDecodeRecordMaskedHostileInput(t *testing.T) {
	masks := []columnMask{allColumns, 0, 1, 1 << 1, 1 << 2, 1<<0 | 1<<2, 1 << 40, 1 << 63}
	for _, hr := range hostileRecords(t) {
		wantErr := func() bool {
			_, err := decodeRecordInto(hr.rec, nil)
			return err != nil
		}()
		if !wantErr {
			t.Fatalf("%s: the unmasked decode ACCEPTED this record -- the fixture is not hostile", hr.name)
		}
		for _, m := range masks {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("%s (mask %#x): PANIC %v", hr.name, uint64(m), r)
					}
				}()
				vals, err := decodeRecordMaskedInto(hr.rec, nil, m)
				if err == nil {
					t.Fatalf("%s (mask %#x): decoded %d columns with no error, want an error",
						hr.name, uint64(m), len(vals))
				}
				if vals != nil {
					t.Fatalf("%s (mask %#x): returned a row alongside its error", hr.name, uint64(m))
				}
			}()
		}
	}
}

// TestDecodeRecordMaskedTruncationSweep is the systematic form of the hostile
// battery: EVERY prefix of a well-formed record, under every single-column
// mask, must either decode identically to the unmasked decode of that same
// prefix or fail identically -- and must never panic. This is the case that
// catches an unvalidated skip, because a prefix cut inside column i's body
// leaves column i+1's offset past the end.
func TestDecodeRecordMaskedTruncationSweep(t *testing.T) {
	rec := encodeRecord(maskTestRow())
	ncols := len(maskTestRow())
	for cut := 0; cut < len(rec); cut++ {
		prefix := rec[:cut]
		fullVals, fullErr := decodeRecordInto(prefix, nil)
		for i := 0; i <= ncols; i++ {
			m := columnMask(1) << uint(i)
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("cut=%d mask=col%d: PANIC %v", cut, i, r)
					}
				}()
				got, err := decodeRecordMaskedInto(prefix, nil, m)
				if (err == nil) != (fullErr == nil) {
					t.Fatalf("cut=%d mask=col%d: masked err %v, unmasked err %v", cut, i, err, fullErr)
				}
				if err != nil {
					return
				}
				if len(got) != len(fullVals) {
					t.Fatalf("cut=%d mask=col%d: %d columns, unmasked gave %d", cut, i, len(got), len(fullVals))
				}
				if i < len(got) && !valuesEqualExact(got[i], fullVals[i]) {
					t.Fatalf("cut=%d mask=col%d: column %d = %v, want %v", cut, i, i, got[i], fullVals[i])
				}
			}()
		}
	}
}

// ---- the cursor-level discipline: a WRONG mask must cost time, not answers ----

// maskScanDB builds a table whose rows exercise the shapes a masked scan has
// to keep exact: an INTEGER PRIMARY KEY whose stored value is always NULL, a
// REAL-affinity column stored as an integer, NULLs, every text/blob width
// including one big enough to overflow onto a second page, and a row narrower
// than the table (via ALTER TABLE ADD COLUMN).
func maskScanDB(t *testing.T) *ReadOnlyPager {
	t.Helper()
	path := t.TempDir() + "/mask.musq"
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, a INTEGER, r REAL, s TEXT, b BLOB)`,
		`INSERT INTO t(id,a,r,s,b) VALUES(1, 10, 2, 'hello', x'00ff10')`,
		`INSERT INTO t(id,a,r,s,b) VALUES(2, NULL, NULL, NULL, NULL)`,
		`INSERT INTO t(id,a,r,s,b) VALUES(3, -9223372036854775808, 1.5, '', x'')`,
		`INSERT INTO t(id,a,r,s,b) VALUES(4, 7, 0, '` + strings.Repeat("w", 6000) + `', x'0102')`,
		`INSERT INTO t(id,a,r,s,b) VALUES(5, 1, -3, '` + strings.Repeat("z", 57) + `', x'03')`,
		`INSERT INTO t(id,a,r,s,b) VALUES(6, 0, 4, '` + strings.Repeat("q", 58) + `', x'04')`,
		`ALTER TABLE t ADD COLUMN late TEXT DEFAULT 'dflt'`,
		`INSERT INTO t(id,a,r,s,b,late) VALUES(7, 2, 5, 'seven', x'05', 'given')`,
	}
	for _, s := range stmts {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// TestCursorColMaskIsOnlyAHint is the safety-valve test, and the one that
// makes the whole optimization safe to ship: it drives a streaming cursor with
// a DELIBERATELY WRONG mask -- every mask from "want nothing" through every
// single-column mask -- and reads every column of every row, in order, out of
// order, and repeatedly. A mask that fails to name a column must still produce
// that column's true value (vdbeCursor.col re-decodes), never the undecoded
// slot's NULL.
//
// It also proves fullRow() completes a masked row, and that a completed row
// stays completed for the rest of that row's life.
func TestCursorColMaskIsOnlyAHint(t *testing.T) {
	p := maskScanDB(t)
	tbl, err := p.resolveTable("t")
	if err != nil {
		t.Fatal(err)
	}
	ncols := len(tbl.cols)

	// The reference: an ordinary unmasked streaming scan.
	want := make([][]Value, 0, 8)
	wantRowids := make([]uint64, 0, 8)
	ref := openCursor(p, tbl)
	ref.streamable = true
	if err := ref.rewind(); err != nil {
		t.Fatal(err)
	}
	for ref.advance() {
		row, err := ref.fullRow()
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, append([]Value(nil), row...))
		wantRowids = append(wantRowids, ref.rowid)
	}
	if ref.streamErrPending != nil {
		t.Fatal(ref.streamErrPending)
	}
	if len(want) != 7 {
		t.Fatalf("reference scan produced %d rows, want 7", len(want))
	}

	// Column read orders: forward, reverse, and one with re-reads and gaps.
	orders := [][]int{}
	fwd := make([]int, ncols)
	rev := make([]int, ncols)
	for i := 0; i < ncols; i++ {
		fwd[i] = i
		rev[i] = ncols - 1 - i
	}
	orders = append(orders, fwd, rev)
	mixed := []int{}
	for i := ncols - 1; i >= 0; i -= 2 {
		mixed = append(mixed, i, i, 0, i)
	}
	orders = append(orders, mixed)

	masks := []columnMask{0, allColumns}
	for i := 0; i < ncols; i++ {
		masks = append(masks, columnMask(1)<<uint(i))
	}
	masks = append(masks, 1<<0|1<<3, 1<<2|1<<4)

	for _, mask := range masks {
		for oi, order := range orders {
			for _, useFullRow := range []bool{false, true} {
				name := fmt.Sprintf("mask=%#x/order=%d/fullRow=%v", uint64(mask), oi, useFullRow)
				cur := openCursor(p, tbl)
				cur.streamable = true
				cur.colMask = mask
				if err := cur.rewind(); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				n := 0
				for cur.advance() {
					if n >= len(want) {
						t.Fatalf("%s: more rows than the reference scan", name)
					}
					if cur.rowid != wantRowids[n] {
						t.Fatalf("%s: row %d rowid %d, want %d", name, n, cur.rowid, wantRowids[n])
					}
					if useFullRow {
						row, ferr := cur.fullRow()
						if ferr != nil {
							t.Fatalf("%s: fullRow: %v", name, ferr)
						}
						if len(row) != len(want[n]) {
							t.Fatalf("%s: row %d has %d columns, want %d", name, n, len(row), len(want[n]))
						}
						for c := range row {
							if !valuesEqualExact(row[c], want[n][c]) {
								t.Fatalf("%s: row %d col %d = %v, want %v", name, n, c, row[c], want[n][c])
							}
						}
					}
					for _, c := range order {
						got, cerr := cur.col(c)
						if cerr != nil {
							t.Fatalf("%s: col(%d): %v", name, c, cerr)
						}
						if !valuesEqualExact(got, want[n][c]) {
							t.Fatalf("%s: row %d col %d = %v, want %v", name, n, c, got, want[n][c])
						}
					}
					n++
				}
				if cur.streamErrPending != nil {
					t.Fatalf("%s: %v", name, cur.streamErrPending)
				}
				if n != len(want) {
					t.Fatalf("%s: scanned %d rows, want %d", name, n, len(want))
				}
			}
		}
	}
}

// TestMaskedScanMatchesUnmaskedSQL is the end-to-end form: every projection of
// the fixture table, run as real SQL through the VDBE (so the mask really is
// the one cursorColumnMasks derived from the compiled program), must return
// exactly what "SELECT *" returns for those same columns.
func TestMaskedScanMatchesUnmaskedSQL(t *testing.T) {
	p := maskScanDB(t)
	names := []string{"id", "a", "r", "s", "b", "late"}

	_, star, err := p.QueryArgs("SELECT id,a,r,s,b,late FROM t ORDER BY id", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(star) != 7 {
		t.Fatalf("SELECT * returned %d rows, want 7", len(star))
	}

	for i, ni := range names {
		for j, nj := range names {
			q := fmt.Sprintf("SELECT %s, %s FROM t ORDER BY id", ni, nj)
			_, rows, err := p.QueryArgs(q, nil)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			if len(rows) != len(star) {
				t.Fatalf("%s: %d rows, want %d", q, len(rows), len(star))
			}
			for r := range rows {
				if !valuesEqualExact(rows[r][0], star[r][i]) {
					t.Fatalf("%s: row %d first column = %v, want %v", q, r, rows[r][0], star[r][i])
				}
				if !valuesEqualExact(rows[r][1], star[r][j]) {
					t.Fatalf("%s: row %d second column = %v, want %v", q, r, rows[r][1], star[r][j])
				}
			}
		}
	}
}

// TestCursorColumnMasksDerivation pins what the derivation actually produces
// for the shapes it is meant to narrow, and -- more importantly -- that it
// DECLINES for the shapes whose opcodes read whole rows. A decline is only a
// performance choice (col()/fullRow() re-decode either way), but a silent
// decline everywhere would make this whole stage a no-op, so it is pinned.
func TestCursorColumnMasksDerivation(t *testing.T) {
	insns := []Instruction{
		{Op: OpOpenRead, P1: 0},
		{Op: OpColumn, P1: 0, P2: 3},
		{Op: OpColumn, P1: 0, P2: 0},
		{Op: OpRowid, P1: 0, P2: 1},
		{Op: OpNext, P1: 0},
	}
	masks := cursorColumnMasks(insns, 1)
	if masks == nil {
		t.Fatal("a plain scan must produce a mask")
	}
	if masks[0] != 1<<0|1<<3 {
		t.Fatalf("mask = %#x, want cols {0,3}", uint64(masks[0]))
	}

	for _, bail := range []OpCode{OpAggStep, OpHashAggStep, OpMatch, OpFts3Aux, OpFts5Aux,
		OpSubquery, OpExists, OpInSub, OpRowSub, OpOpenDerived, OpOuterColumn, OpOuterRowid} {
		with := append(append([]Instruction(nil), insns...), Instruction{Op: bail})
		if got := cursorColumnMasks(with, 1); got != nil {
			t.Fatalf("%v must make the derivation decline, got %#x", bail, uint64(got[0]))
		}
	}

	// A column number out of the range a column mask can express declines
	// rather than folding onto some bit.
	if got := cursorColumnMasks([]Instruction{{Op: OpColumn, P1: 0, P2: -1}}, 1); got != nil {
		t.Fatal("a negative column number must make the derivation decline")
	}
	// Column 63 and above share bit 63.
	got := cursorColumnMasks([]Instruction{{Op: OpColumn, P1: 0, P2: 200}}, 1)
	if got == nil || got[0] != 1<<63 {
		t.Fatalf("column 200 must set bit 63, got %v", got)
	}
	// A cursor no OpColumn names wants nothing.
	got = cursorColumnMasks([]Instruction{{Op: OpColumn, P1: 0, P2: 1}}, 2)
	if got == nil || got[1] != 0 {
		t.Fatalf("an unread cursor's mask should be empty, got %v", got)
	}
}

// TestGeneratedColumnsAreNeverMasked pins the correctness exclusion in
// OpOpenRead: a generated column is COMPUTED from its siblings rather than
// read, so a mask that skipped those siblings would compute it from undecoded
// NULLs and store that as the row's real answer -- which no later re-decode
// would revisit, because the column IS in the mask.
func TestGeneratedColumnsAreNeverMasked(t *testing.T) {
	path := t.TempDir() + "/gen.musq"
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE g(a INTEGER, b INTEGER, v INTEGER AS (a+b) VIRTUAL, s INTEGER AS (a*b) STORED)`,
		`INSERT INTO g(a,b) VALUES(2,3)`,
		`INSERT INTO g(a,b) VALUES(10,20)`,
		`INSERT INTO g(a,b) VALUES(NULL,1)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// Reading ONLY the generated columns is the shape that would break: their
	// inputs are not in the mask.
	_, rows, err := p.QueryArgs("SELECT v, s FROM g ORDER BY a", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]any{{nil, nil}, {int64(5), int64(6)}, {int64(30), int64(200)}}
	if len(rows) != len(want) {
		t.Fatalf("%d rows, want %d", len(rows), len(want))
	}
	for i, w := range want {
		for c, wv := range [2]any{w[0], w[1]} {
			if wv == nil {
				if rows[i][c].Typ != Null {
					t.Fatalf("row %d col %d = %v, want NULL", i, c, rows[i][c])
				}
				continue
			}
			if rows[i][c].Typ != Int || rows[i][c].I != wv.(int64) {
				t.Fatalf("row %d col %d = %v, want %v", i, c, rows[i][c], wv)
			}
		}
	}
}

// TestWithoutRowidScanUnaffected pins that a WITHOUT ROWID table -- whose own
// b-tree is an INDEX b-tree that the streaming scan cannot read at all, and
// whose rows are PERMUTED into table-column order (permuteWithoutRowidRow), so
// record positions are not column positions -- still answers exactly.
func TestWithoutRowidScanUnaffected(t *testing.T) {
	path := t.TempDir() + "/wr.musq"
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE w(a TEXT, b INTEGER, c TEXT, PRIMARY KEY(b)) WITHOUT ROWID`,
		`INSERT INTO w VALUES('x', 2, 'p')`,
		`INSERT INTO w VALUES('y', 1, 'q')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	_, rows, err := p.QueryArgs("SELECT c, a FROM w ORDER BY b", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"q", "y"}, {"p", "x"}}
	if len(rows) != len(want) {
		t.Fatalf("%d rows, want %d", len(rows), len(want))
	}
	for i, w := range want {
		if string(rows[i][0].S) != w[0] || string(rows[i][1].S) != w[1] {
			t.Fatalf("row %d = %q,%q want %q,%q", i, rows[i][0].S, rows[i][1].S, w[0], w[1])
		}
	}
}

// TestMaskFiresForARealProjectionScan is the fires/no-fire structural pin, in
// the idiom TestIndexSeekFiresWhenEligible uses for the seek hints: it proves
// the mask really narrows for the shapes this stage exists to speed up, and
// really declines for the ones it must not touch. Without it every other test
// here would still pass with the whole optimization disabled -- the safety
// valve makes a never-firing mask indistinguishable by RESULTS alone.
func TestMaskFiresForARealProjectionScan(t *testing.T) {
	p := maskScanDB(t)
	tbl, err := p.resolveTable("t")
	if err != nil {
		t.Fatal(err)
	}
	ncols := len(tbl.cols)

	maskFor := func(sqlText string) (columnMask, bool) {
		t.Helper()
		stmt, perr := ParseSelect(sqlText)
		if perr != nil {
			t.Fatalf("%s: %v", sqlText, perr)
		}
		prog, cerr := compileSelectScan(p, stmt, nil)
		if cerr != nil {
			t.Fatalf("%s: %v", sqlText, cerr)
		}
		streamable := false
		for _, in := range prog.Insns {
			if in.Op == OpOpenRead && in.P1 == 0 && in.P5 != 0 {
				streamable = true
			}
		}
		masks := cursorColumnMasks(prog.Insns, prog.NCursors)
		if masks == nil {
			return allColumns, streamable
		}
		return masks[0].widenIfTotal(ncols), streamable
	}

	// A narrow projection over a streamable full scan: the shape the whole
	// stage is for. Column 3 is "s".
	m, streamable := maskFor("SELECT s FROM t")
	if !streamable {
		t.Fatal("SELECT s FROM t: cursor 0 is not streamable, so nothing would mask")
	}
	if m != 1<<3 {
		t.Fatalf("SELECT s FROM t: mask %#x, want only column 3", uint64(m))
	}

	// Two columns, one of them the INTEGER PRIMARY KEY.
	if m, _ = maskFor("SELECT id, b FROM t"); m != 1<<0|1<<4 {
		t.Fatalf("SELECT id, b FROM t: mask %#x, want columns {0,4}", uint64(m))
	}

	// A WHERE-only reference still counts -- it is an OpColumn like any other.
	if m, _ = maskFor("SELECT s FROM t WHERE a > 1"); m != 1<<1|1<<3 {
		t.Fatalf("SELECT s FROM t WHERE a > 1: mask %#x, want columns {1,3}", uint64(m))
	}

	// Every column named: collapses back to the unmasked path.
	if m, _ = maskFor("SELECT id,a,r,s,b,late FROM t"); m != allColumns {
		t.Fatalf("a whole-row projection should widen to allColumns, got %#x", uint64(m))
	}

	// An aggregate reads whole rows through gatherCursorRow, so the derivation
	// declines outright -- W4/W5 of the benchmark suite are exactly this, and
	// are honestly NOT sped up by this stage.
	if m, _ = maskFor("SELECT count(*) FROM t WHERE a > 1"); m != allColumns {
		t.Fatalf("an aggregate scan must decline to mask, got %#x", uint64(m))
	}
	if m, _ = maskFor("SELECT a, count(*) FROM t GROUP BY a"); m != allColumns {
		t.Fatalf("a GROUP BY scan must decline to mask, got %#x", uint64(m))
	}
}

// TestOneByteSerialTypeBoundary pins the exact boundary of the one-byte
// serial-type fast path (vdbe.c:3126's `(pC->aType[i] = t = zHdr[0])<0x80`).
// 0x80 is the first byte value that is NOT a whole serial type: its
// continuation bit is set, so the varint decoder must be entered. SQLite's own
// encoder never emits a leading 0x80 (that would be a non-minimal encoding of
// a value below 128, and putVarint's two-byte form always starts at 0x81), so
// this boundary is only observable on a record from somewhere else -- exactly
// the input "never wrong" has to hold for.
func TestOneByteSerialTypeBoundary(t *testing.T) {
	// Header length 3; one serial type written as the non-minimal two-byte
	// varint 0x80 0x09 -- serial type 9, the integer constant 1, no body.
	rec := []byte{0x03, 0x80, 0x09}
	for _, m := range []columnMask{allColumns, 0, 1} {
		vals, err := decodeRecordMaskedInto(rec, nil, m)
		if err != nil {
			t.Fatalf("mask %#x: %v", uint64(m), err)
		}
		if len(vals) != 1 {
			t.Fatalf("mask %#x: %d columns, want 1 (0x80 must not be read as a serial type of its own)",
				uint64(m), len(vals))
		}
		if m.has(0) && (vals[0].Typ != Int || vals[0].I != 1) {
			t.Fatalf("mask %#x: column 0 = %v, want the integer 1", uint64(m), vals[0])
		}
	}
	// And 0x7f, the last byte value that IS a whole serial type: TEXT of
	// (127-13)/2 = 57 bytes.
	rec = append([]byte{0x02, 0x7f}, bytes.Repeat([]byte("k"), 57)...)
	vals, err := decodeRecordInto(rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 1 || vals[0].Typ != Text || len(vals[0].S) != 57 {
		t.Fatalf("serial 0x7f decoded to %v, want a 57-byte TEXT", vals)
	}
}

// A closed cursor must not be able to answer a column from a MASKED row.
//
// readColumn decides whether a row still needs completing by testing
// `cur.rowRaw != nil`. close() cleared rowRaw but left rowVals holding the
// masked row, so a read after close took the fast path and handed back the
// undecoded slot -- a silent NULL for any column outside the mask, which is
// precisely the failure the whole mask design is built to make impossible.
// close() now clears both halves.
func TestClosedCursorCannotAnswerFromAMaskedRow(t *testing.T) {
	cur := &vdbeCursor{
		rowVals: []Value{{Typ: Int, I: 1}, {}, {}, {Typ: Int, I: 4}},
		rowRaw:  []byte{0x01}, // non-nil: "rowVals is masked and incomplete"
		colMask: 1 << 0,       // only column 0 was decoded
	}
	cur.close()
	if cur.rowRaw != nil {
		t.Error("close() left rowRaw set")
	}
	if cur.rowVals != nil {
		t.Fatal("close() left rowVals set: a later readColumn would take the " +
			"unmasked fast path and answer an undecoded column with its slot's NULL")
	}
}
