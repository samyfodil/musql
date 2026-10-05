// This file owns fts5's LOCALES: the locale=1 option, which makes a table store
// a per-column locale string beside each value, and the two SQL functions that
// put one in and take one out -- fts5_locale(LOCALE, TEXT) and
// fts5_get_locale(<table>, iCol).
//
// # What the option changes, and what it does not
//
// Read off ext/fts5/*.c at the oracle's exact source id (3.53.3) and confirmed
// against it:
//
//   - %_content grows one extra column, "l<i>", for each column that is NOT
//     UNINDEXED, appended after every "c<i>" column (fts5_storage.c:378, whose
//     guard is "pConfig->bLocale" then "abUnindexed[i]==0"). Verified: fts5(x,
//     y UNINDEXED, locale=1) declares 't1_content'(id INTEGER PRIMARY KEY, c0,
//     c1, l0) -- an l for the INDEXED x and none for the UNINDEXED y -- and
//     fts5(a UNINDEXED, b UNINDEXED, locale=1) declares (id, c0, c1) with no l
//     column at all.
//   - NOTHING ELSE. %_data, %_docsize and %_config are BYTE-IDENTICAL to the
//     same table declared locale=0: verified by filling fts5(a,b,locale=1) and
//     fts5(a,b) with the same two rows and comparing each shadow table's whole
//     contents (all three compared equal). That is not a coincidence of the
//     fixture -- ext/fts5/fts5_tokenize.c never mentions pLocale, so ascii,
//     unicode61 and trigram are locale-BLIND and only a custom v2 tokenizer
//     (which no SQL statement can install) can read one. The locale therefore
//     never reaches a single index byte.
//
// So a locale=1 table is exactly a locale=0 table plus a column of %_content
// this engine has to carry faithfully -- which is what the helpers below do.
//
// # The locale VALUE, and fts5_locale()
//
// The only way to PUT a locale into a row is the fts5_locale(LOCALE, TEXT)
// scalar function (fts5_main.c:3644, fts5LocaleFunc). It returns TEXT
// unchanged, as text, when LOCALE is NULL or zero-length; otherwise a BLOB of
//
//	<16-byte header><LOCALE utf-8>0x00<TEXT utf-8>
//
// with no trailing NUL. The header is Fts5Global.aLocaleHdr, a 128-bit vector
// seeded once PER sqlite3* CONNECTION from sqlite3_randomness xor'd with four
// constants (fts5_main.c:3806). The in-source comment above fts5LocaleFunc,
// which describes a fixed 4-byte 0x00,0xE0,0xB2,0xEB header, is STALE -- the
// code memcpy's all 16 bytes of the random vector, and verified against the
// oracle a blob whose first four bytes are exactly 00 E0 B2 EB and nothing
// more is stored verbatim as a plain blob, locale NULL.
//
// A blob is a locale value if and only if it carries THAT connection's header
// (sqlite3Fts5IsLocaleValue, fts5_main.c:1338), so the marker is a runtime
// secret: it never appears in a file, and it never survives a reconnect.
//
// THIS ENGINE SCOPES THE HEADER PER PROCESS, not per connection, because the
// scalar-dispatch calling convention (callScalarFuncEnc) carries no
// *DB. That is a strictly WIDER secret, and the one shape it decides
// differently is a locale value produced on one connection and inserted from
// ANOTHER one in the same process: C SQLite stores that as a plain blob,
// this engine would read it as a locale. Nothing in the corpus does it, and
// narrowing it means threading a *DB through every scalar call. Recorded here
// rather than hidden: if that shape ever matters, the fix is to move
// fts5LocaleHeader onto DB and thread it, not to change the encoding.
//
// # Preservation, which is the part that can go wrong
//
// Real fts5 rewrites %_content row by row and binds l<i> from the SAVED ROW
// whenever sqlite3_value_nochange() says the UPDATE did not name column i
// (fts5_storage.c:994) -- and leaves it UNBOUND, i.e. NULL, when the column
// WAS named with a value that is not a locale value (the bindings were cleared
// first). Verified end to end: over a row holding ('one two','three') with
// locales ('en','fr'), "UPDATE v1 SET b='changed'" leaves l0='en' and sets
// l1=NULL. fts5LocaleAfterUpdate below is exactly that rule.
package engine

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
)

// fts5LocaleHdrSize is FTS5_LOCALE_HDR_SIZE (fts5_main.c:93): sizeof
// Fts5Global.aLocaleHdr, four u32 = 16 bytes.
const fts5LocaleHdrSize = 16

// fts5LocaleHeader returns this process's locale-value header -- the analogue
// of Fts5Global.aLocaleHdr. See the file doc comment for why it is per-process
// here and per-connection in C SQLite.
//
// It is RANDOM rather than a fixed constant for the reason the C's is: a blob
// is treated as a locale value purely by carrying the header, so a fixed one
// would silently reinterpret an ordinary user blob that happened to start with
// those bytes.
var fts5LocaleHeader = sync.OnceValue(func() []byte {
	b := make([]byte, fts5LocaleHdrSize)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read never returns an error on any supported platform
		// (it panics internally on a broken source). Falling back to a fixed
		// vector here would be worse than useless -- it is exactly the fixed
		// header the randomness exists to avoid -- so surface it as a header
		// nothing can match: an all-zero header still requires 16 leading NUL
		// bytes plus a NUL terminator, which no fts5 test value carries.
		for i := range b {
			b[i] = 0
		}
	}
	return b
})

// fts5LocaleFuncName reports whether name is fts5_locale(), one of the four
// scalar functions the fts5 extension registers that are not auxiliary
// functions (fts5_main.c:3821-3845; fts5ScalarFuncArity in scalar_ext_funcs.go
// covers all four, and the aux ones go through fts5AuxFuncName instead).
//
// It is a PREDICATE rather than an entry in supportedFuncs
// because that map is static and RegisterFTS5 is opt-in: the project's default
// CGo oracle is built without fts5, so an unconditional entry would make the
// default build accept "fts5_locale()" where the default oracle answers
// "no such function" -- a wrong answer in the build the corpus does NOT skip.
// C SQLite registers the function from fts5Init, i.e. exactly when the
// module is there, and this mirrors that.
func fts5LocaleFuncName(name string) bool {
	if !strings.EqualFold(name, "fts5_locale") {
		return false
	}
	_, ok := vtabModules[r33sFoldIdent("fts5")]
	return ok
}

// fts5LocaleFunc implements fts5_locale(LOCALE, TEXT), a byte-for-byte port of
// fts5LocaleFunc (fts5_main.c:3644).
//
// Both arguments are read as TEXT (sqlite3_value_text coerces), so
// fts5_locale(NULL,123) is the text '123'. A NULL or zero-length LOCALE yields
// TEXT unchanged -- and when TEXT is itself NULL that is sqlite3_result_text
// with a null pointer, i.e. NULL rather than the empty string. Verified
// against the oracle: typeof(fts5_locale(NULL,'xyz')) is 'text', and so is
// typeof(fts5_locale(<empty string>,'abc')).
func fts5LocaleFunc(args []Value) (Value, error) {
	if len(args) != 2 {
		return Value{}, fmt.Errorf("engine: wrong number of arguments to function fts5_locale()")
	}
	if args[0].Typ == Null || len(valueToText(args[0])) == 0 {
		if args[1].Typ == Null {
			return Value{}, nil
		}
		return Value{Typ: Text, S: []byte(valueToText(args[1]))}, nil
	}
	loc := []byte(valueToText(args[0]))
	var text []byte
	if args[1].Typ != Null {
		text = []byte(valueToText(args[1]))
	}
	blob := make([]byte, 0, fts5LocaleHdrSize+len(loc)+1+len(text))
	blob = append(blob, fts5LocaleHeader()...)
	blob = append(blob, loc...)
	blob = append(blob, 0x00)
	blob = append(blob, text...)
	return Value{Typ: Blob, S: blob}, nil
}

// fts5IsLocaleValue is sqlite3Fts5IsLocaleValue (fts5_main.c:1338): a BLOB
// strictly longer than the header and carrying the header. That is the whole
// test -- it deliberately does NOT check that a 0x00 locale terminator follows,
// which is why the decode below can fail on a value this accepts.
func fts5IsLocaleValue(v Value) bool {
	return v.Typ == Blob && len(v.S) > fts5LocaleHdrSize &&
		bytes.Equal(v.S[:fts5LocaleHdrSize], fts5LocaleHeader())
}

// fts5DecodeLocaleValue is sqlite3Fts5DecodeLocaleValue (fts5_main.c:1371):
// it splits a locale value into the TEXT that gets indexed and stored, and the
// LOCALE that goes into %_content's l<i> column. v must satisfy
// fts5IsLocaleValue.
//
// ok is false when there is no 0x00 after the header, which the C answers
// SQLITE_MISMATCH -- an ERROR, not "then it was not a locale value after all".
// The distinction is reachable: fts5_locale() always writes the terminator, but
// "substr(fts5_locale('en','x'),1,17)" is a header-carrying blob without one,
// and treating that as an ordinary blob would STORE it where C fts5 refuses
// the statement.
//
// An empty locale (the terminator immediately after the header) is NOT an
// error -- the C's loop simply exits with nLoc==0 and binds a zero-length
// locale. fts5_locale() itself cannot make one, since an empty LOCALE takes its
// text branch; only a hand-built blob can.
func fts5DecodeLocaleValue(v Value) (text, locale Value, ok bool) {
	body := v.S[fts5LocaleHdrSize:]
	n := bytes.IndexByte(body, 0x00)
	if n < 0 {
		return Value{}, Value{}, false
	}
	return Value{Typ: Text, S: append([]byte(nil), body[n+1:]...)},
		Value{Typ: Text, S: append([]byte(nil), body[:n]...)}, true
}

// fts5LocaleUnwrapRow replaces every fts5_locale() value in one row with the
// TEXT it wraps, and returns the row's locales (nil when it has none). The
// input row is left untouched: a copy is made only when there is something to
// unwrap, so the overwhelmingly common locale-free write allocates nothing.
//
// vals is the fts5 row shape [rowid, col0, ...], so column i is vals[i+1].
//
// The locale=0 rejection is fts5UpdateMethod's (fts5_main.c:2030): writing a
// locale value into a table without locale=1 is SQLITE_MISMATCH with the
// message below, checked over every column BEFORE anything is stored. An
// UNINDEXED column has no l<i> column to hold a locale (fts5_storage.c:378),
// so a locale value there is unwrapped to its text and the locale DROPPED --
// which is what C fts5 does, since the binding it would go into does not
// exist.
func fts5LocaleUnwrapRow(st *fts5Store, vals []Value) ([]Value, []Value, error) {
	var locs []Value
	out := vals
	for i := range st.colNames {
		j := i + 1
		if j >= len(out) || !fts5IsLocaleValue(out[j]) {
			continue
		}
		if !st.locale {
			return nil, nil, fmt.Errorf("fts5_locale() requires locale=1")
		}
		text, loc, decoded := fts5DecodeLocaleValue(out[j])
		if !decoded {
			return nil, nil, fmt.Errorf("datatype mismatch")
		}
		if locs == nil {
			locs = make([]Value, len(st.colNames))
			out = append([]Value(nil), vals...)
		}
		out[j] = text
		if st.unindexed[i] {
			continue
		}
		locs[i] = loc
	}
	return out, locs, nil
}

// fts5LocaleAppendColumns appends %_content's "l<i>" column declarations for a
// locale=1 table -- one per INDEXED column, after every "c<i>" column. It is a
// no-op for locale=0, and for a table whose every column is UNINDEXED.
func fts5LocaleAppendColumns(b *strings.Builder, st *fts5Store) {
	if !st.locale {
		return
	}
	for i := range st.colNames {
		if st.unindexed[i] {
			continue
		}
		fmt.Fprintf(b, ", l%d", i)
	}
}

// fts5LocaleAppendRecord appends one row's stored locales to the %_content
// record being built for it, in the same order fts5LocaleAppendColumns
// declared them.
func fts5LocaleAppendRecord(rec []Value, st *fts5Store, rowid int64) []Value {
	if !st.locale {
		return rec
	}
	locs := st.locales[rowid]
	for i := range st.colNames {
		if st.unindexed[i] {
			continue
		}
		var v Value
		if i < len(locs) {
			v = locs[i]
		}
		rec = append(rec, v)
	}
	return rec
}

// fts5LocaleSplitRecord splits a %_content record read back from a file into
// the row's VALUES (the "c<i>" columns, which is what every other read path
// wants) and its LOCALES (the "l<i>" columns, widened back to one slot per
// declared column so that st.locales stays parallel to colNames).
//
// record is the whole stored record INCLUDING slot 0, which %_content carries
// as its "id INTEGER PRIMARY KEY" and whose value the b-tree key supplies.
func fts5LocaleSplitRecord(st *fts5Store, record []Value) (vals, locs []Value) {
	body := record[1:]
	if !st.locale {
		return body, nil
	}
	// The "c<i>" columns come first, and there is one per column C fts5
	// STORES -- every column for FTS5_CONTENT_NORMAL, only the UNINDEXED ones
	// for FTS5_CONTENT_UNINDEXED (createShadowTables says the same thing from
	// the other side).
	nContent := 0
	for i := range st.colNames {
		if st.contentUnindexed && !st.unindexed[i] {
			continue
		}
		nContent++
	}
	if len(body) < nContent {
		// A record too short to hold even its values: leave it to the ordinary
		// short-record handling (every reader pads), with no locales.
		return body, nil
	}
	vals, tail := body[:nContent], body[nContent:]
	locs = make([]Value, len(st.colNames))
	j := 0
	for i := range st.colNames {
		if st.unindexed[i] {
			continue
		}
		if j < len(tail) {
			locs[i] = tail[j]
		}
		j++
	}
	return vals, locs
}

// fts5LocaleAfterUpdate is the locale row an UPDATE leaves behind: the old
// row's locale for every column the SET list did NOT name, and NULL for every
// one it did.
//
// modified is vtab_write.go's SET-list mask over the store's DECLARED columns,
// whose slot 0 is the hidden rowid -- so fts5 column i is slot i+1, exactly as
// UpdateRowSet reads it. A nil result means "no locales", which keeps the map
// free of all-NULL entries.
func fts5LocaleAfterUpdate(st *fts5Store, oldRowid int64, modified []bool) []Value {
	if !st.locale {
		return nil
	}
	old := st.locales[oldRowid]
	if len(old) == 0 {
		return nil
	}
	out := make([]Value, len(st.colNames))
	any := false
	for i := range st.colNames {
		if st.unindexed[i] || i >= len(old) || old[i].Typ == Null {
			continue
		}
		if i+1 < len(modified) && modified[i+1] {
			continue // the SET list named it: C fts5 binds no locale
		}
		out[i] = old[i]
		any = true
	}
	if !any {
		return nil
	}
	return out
}

// fts5LocaleUnwrapExtRow is fts5LocaleUnwrapRow for an EXTERNAL-content
// table's projected row: vals is rewritten in place (it was just built by
// fts5ExtProject, so there is no caller to surprise) and the locales are
// recorded on the row set rather than on a store.
//
// It differs from the internal-content rule in the one way the C does
// (fts5_main.c:2264 against fts5_storage.c:553): there is no "fts5_locale()
// requires locale=1" here. That rejection belongs to fts5UpdateMethod, which an
// external-content table's writes never go through -- rows arrive by an
// ordinary INSERT into the content table, which fts5 does not see. So a
// locale=0 external table simply keeps the blob verbatim, and only a locale=1
// one unwraps it. vals is indexed by fts5 COLUMN (no leading rowid slot).
func fts5LocaleUnwrapExtRow(st *fts5Store, out *fts5ExtRows, rowid int64, vals []Value) error {
	if !st.locale {
		return nil
	}
	var locs []Value
	for i := range vals {
		if !fts5IsLocaleValue(vals[i]) {
			continue
		}
		text, loc, decoded := fts5DecodeLocaleValue(vals[i])
		if !decoded {
			return fmt.Errorf("datatype mismatch")
		}
		vals[i] = text
		if i < len(st.unindexed) && st.unindexed[i] {
			continue
		}
		if locs == nil {
			locs = make([]Value, len(vals))
		}
		locs[i] = loc
	}
	if locs == nil {
		return nil
	}
	if out.locales == nil {
		out.locales = map[int64][]Value{}
	}
	out.locales[rowid] = locs
	return nil
}

// fts5LocaleMerge lays the locales an UPDATE's SET list GAVE over the ones its
// old row kept. Only a non-NULL given locale wins: a SET list that named a
// column with an ordinary value leaves that column locale-less, which
// fts5LocaleAfterUpdate has already expressed as a NULL slot.
func fts5LocaleMerge(kept, given []Value) []Value {
	if len(given) == 0 {
		return kept
	}
	if len(kept) < len(given) {
		out := make([]Value, len(given))
		copy(out, kept)
		kept = out
	}
	for i, v := range given {
		if v.Typ != Null {
			kept[i] = v
		}
	}
	return kept
}

// fts5LocaleSet records (or clears) one row's locales. Clearing rather than
// storing an all-NULL slice keeps "the map has an entry" meaning "this row has
// at least one locale".
func fts5LocaleSet(st *fts5Store, rowid int64, locs []Value) {
	if !st.locale {
		return
	}
	any := false
	for _, v := range locs {
		if v.Typ != Null {
			any = true
			break
		}
	}
	if !any {
		delete(st.locales, rowid)
		return
	}
	if st.locales == nil {
		st.locales = map[int64][]Value{}
	}
	st.locales[rowid] = locs
}

// fts5LocalesOf reads the per-row locales of fts5 table name out of this read
// snapshot's %_content, keyed by rowid and indexed by DECLARED column (so a
// column with no locale, and every UNINDEXED one, is a Null slot).
//
// A nil map means "this table holds no locales", which every reader treats as
// all-NULL. That covers a locale=0 table and a CONTENTLESS one: fts5_config.c
// only ever declares l<i> for a NORMAL/UNINDEXED-content table, and a
// contentless table has no %_content at all, so C fts5's
// fts5_get_locale() has nothing to return there either.
//
// An EXTERNAL-content table is the exception, and it is not nil: it has no
// l<i> column, but its locales live INSIDE the content table's own values and
// fts5ExtProject has already unwrapped them (fts5_extcontent.go).
func (p *ReadOnlyPager) fts5LocalesOf(name string) (map[int64][]Value, error) {
	sch, ok := p.fts5SchemaTok(name)
	if !ok || !sch.locale || sch.cl != nil {
		return nil, nil
	}
	if sch.extContent != "" {
		st, err := p.fts5ExtStoreOf(name)
		if err != nil || st == nil || st.extSource == nil {
			return nil, err
		}
		return st.extSource.locales, nil
	}
	rows, err := p.Schema()
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.Type != "table" || !strings.EqualFold(r.Name, name) || !isCreateVirtualTableSQL(r.SQL) {
			continue
		}
		_, mod, args, _, perr := parseCreateVirtualTableStmt(r.SQL)
		if perr != nil || !strings.EqualFold(mod, "fts5") {
			continue
		}
		st, serr := (fts5Module{}).buildStore(r.Name, args)
		if serr != nil {
			return nil, serr
		}
		rowids, records, rerr := p.Rows(r.Name + "_content")
		if rerr != nil {
			return nil, rerr
		}
		out := map[int64][]Value{}
		for i, rid := range rowids {
			_, locs := fts5LocaleSplitRecord(st, records[i])
			for _, v := range locs {
				if v.Typ != Null {
					out[int64(rid)] = locs
					break
				}
			}
		}
		return out, nil
	}
	return nil, nil
}

// fts5LocaleDeclineUnindexedContentless is the one locale= combination this
// engine refuses: locale=1 together with the contentless_unindexed=1 content
// mode. Real fts5 ACCEPTS that CREATE and then fails every INSERT into the
// table with "database disk image is malformed" -- verified against the oracle
// for a plain (locale-free) insert, and it is a C fts5 defect, not a rule:
// sqlite3Fts5StorageOpen declares %_content as (id, c1, l0) for
// fts5(a, b UNINDEXED, locale=1, an EMPTY content=, contentless_unindexed=1) but
// fts5StorageGetStmt builds that table's INSERT bindings under
// "bLocale && eContent==FTS5_CONTENT_NORMAL" (fts5_storage.c:165), which is
// false here, so the statement never has a binding for l0 and the widths
// disagree.
//
// Reproducing a table that can be created and never written is not worth a
// mode of its own; the CREATE is declined instead, which is a gap and not a
// wrong answer.
func fts5LocaleDeclineUnindexedContentless(locale, contentUnindexed bool) error {
	if !locale || !contentUnindexed {
		return nil
	}
	return fmt.Errorf("fts5: locale=1 together with contentless_unindexed=1 is not supported by this engine (C fts5 creates that table with a %%_content its own INSERT statement has no binding for, and every write of it is \"database disk image is malformed\")")
}
