// This file tests fixes for FTS3 position list matching and segment decoding.
package compat

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// fts3PoslistBLMSetupAndCompare creates identical fts3 tables in both
// engines from setup, runs query against each, and asserts a full byte-exact
// match (queryResultsMatch), returning the engine's own rows for any further
// per-test assertion.
func fts3PoslistBLMSetupAndCompare(t *testing.T, name string, setup []string, query string) [][]string {
	t.Helper()
	path := t.TempDir() + "/" + name + ".sqlite"
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range setup {
		if err := db.Exec(s); err != nil {
			db.Close()
			t.Fatalf("engine setup Exec(%.80s): %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cpath := t.TempDir() + "/" + name + "_c.sqlite"
	cdb, err := sql.Open("sqlite3", cpath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cdb.Close() })
	for _, s := range setup {
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo setup Exec(%.80s): %v", s, err)
		}
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })

	vCols, vVals, vErr := p.QueryArgs(query, nil)
	if vErr != nil {
		if strings.Contains(vErr.Error(), "position list with no positions") {
			t.Fatalf("REGRESSION: still declines on the position-list-with-no-positions check: %v", vErr)
		}
		t.Fatalf("engine declined the mined statement: %v", vErr)
	}
	vRows := engineRowsToStrings(vVals)

	cCols, cRows, cErr := cgoSelect(t, cdb, query, nil)
	if cErr != nil {
		t.Fatalf("oracle rejected its own corpus statement: %v", cErr)
	}
	if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, false); !ok {
		t.Fatalf("DIVERGES from C SQLite: %s\n  engine: %v\n  cgo:    %v", reason, vRows, cRows)
	}
	return vRows
}

// TestFts3PoslistByteLengthMatchQuoteMatchinfo is the exact mined statement
// fts3corrupt4.test 15.1: a prefix MATCH 'e*' whose term "excepteur" has a
// byte-length-only match at docid 7 (see this file's own doc comment, fix
// 1). Guards fix 2 too: without it this still declines on the %_content
// existence cross-check for "quote(matchinfo(t1,t1))==0".
func TestFts3PoslistByteLengthMatchQuoteMatchinfo(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t1 USING fts3(a, content="")`,
		`INSERT INTO t1_segdir VALUES(0,0,0,0,'0 665',X'000261640303040002086970697363696e670301080001056c6971756103020c00050269700304040001036d65740301060001036e6a6d03080900010375746503050300000663696c6c756d0306020001066f6d6d6f646f0304070002096e736563746574757203010700050471756174030408000104756c7061030804000207706964617461740307050000086465736572756e740308070001016f0302030002036c6f720601040004050005016506020a00040300010375697303050200000265610304060001066975736d6f640302040001036c69740301090001036e696d13030300010373736503050b0002017403080b0001017403020900010175030604000101780304050002076365707465757203070100020a65726369746174696f6e030309000006667567696174030605000002696403080a0001016e070506040003030002086369646964756e740302060001047073756d030103000104727572650305040000066c61626f7265030208000502697303030b000502756d03080c0001046f72656d0301020000056d61676e6103020b000104696e696d0303050001056f6c6c69740308080000046e6973690304020001026f6e0307060002057374727564030308000104756c6c610306060000086f636361656361740307040001066666696369610308060000087061726961747572030607000107726f6964656e740307070000037175690308050003017303030700000d726570726568656e6465726974030507000003736564030202000103696e7403070300020174030105000103756e7403080200000674656d706f72030205000007756c6c616d636f03030a0001017409020700010200010300000576656c697403050a0002046e69616d0303060001086f6c75707461746503050900')`,
	}
	query := `SELECT quote(matchinfo(t1, t1 ))==0 FROM t1 WHERE t1 MATCH 'e*'`

	got := fts3PoslistBLMSetupAndCompare(t, "fts3_blm_quote_matchinfo", setup, query)
	// fts3corrupt4.test's own do_execsql_test 15.1 pins the result as
	// {0 0 0 0 0 0}: six rows, one per docid MATCH 'e*' reaches (including
	// docid 7's byte-length-only match), quote()'s blob never equal to 0.
	if len(got) != 6 {
		t.Fatalf("got %d rows, want the 6 fts3corrupt4.test 15.1 pins: %v", len(got), got)
	}
	for i, row := range got {
		if len(row) != 1 || row[0] != "I:0" {
			t.Fatalf("row %d = %v, want a single 0 column", i, row)
		}
	}
}

// TestFts3PoslistByteLengthMatchColumnFilterExcluded is 15.1's own table and
// term, but asserted directly at row-membership granularity (no matchinfo()
// noise): a bare/unfiltered "e*" prefix includes docid 7's byte-length
// match, a real column filter "a:e*" excludes it (no position to satisfy a
// column restriction with) -- the exact pair fts3MergeDoclist's own doc
// comment cites as verified against the oracle.
func TestFts3PoslistByteLengthMatchColumnFilterExcluded(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t1 USING fts3(a, content="")`,
		`INSERT INTO t1_segdir VALUES(0,0,0,0,'0 665',X'000261640303040002086970697363696e670301080001056c6971756103020c00050269700304040001036d65740301060001036e6a6d03080900010375746503050300000663696c6c756d0306020001066f6d6d6f646f0304070002096e736563746574757203010700050471756174030408000104756c7061030804000207706964617461740307050000086465736572756e740308070001016f0302030002036c6f720601040004050005016506020a00040300010375697303050200000265610304060001066975736d6f640302040001036c69740301090001036e696d13030300010373736503050b0002017403080b0001017403020900010175030604000101780304050002076365707465757203070100020a65726369746174696f6e030309000006667567696174030605000002696403080a0001016e070506040003030002086369646964756e740302060001047073756d030103000104727572650305040000066c61626f7265030208000502697303030b000502756d03080c0001046f72656d0301020000056d61676e6103020b000104696e696d0303050001056f6c6c69740308080000046e6973690304020001026f6e0307060002057374727564030308000104756c6c610306060000086f636361656361740307040001066666696369610308060000087061726961747572030607000107726f6964656e740307070000037175690308050003017303030700000d726570726568656e6465726974030507000003736564030202000103696e7403070300020174030105000103756e7403080200000674656d706f72030205000007756c6c616d636f03030a0001017409020700010200010300000576656c697403050a0002046e69616d0303060001086f6c75707461746503050900')`,
	}

	blm := fts3PoslistBLMSetupAndCompare(t, "fts3_blm_bare", setup, `SELECT rowid FROM t1 WHERE t1 MATCH 'e*'`)
	wantBare := []string{"1", "2", "3", "4", "6", "7"}
	if got := poslistRowidCol(blm); !equalStrSlices(got, wantBare) {
		t.Fatalf("bare 'e*': got %v, want %v", got, wantBare)
	}

	colf := fts3PoslistBLMSetupAndCompare(t, "fts3_blm_colfilter", setup, `SELECT rowid FROM t1 WHERE t1 MATCH 'a:e*'`)
	wantFiltered := []string{"1", "2", "3", "4", "6"}
	if got := poslistRowidCol(colf); !equalStrSlices(got, wantFiltered) {
		t.Fatalf("column-filtered 'a:e*': got %v, want %v (docid 7 must NOT appear -- no real position)", got, wantFiltered)
	}
}

func poslistRowidCol(rows [][]string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = strings.TrimPrefix(r[0], "I:")
	}
	return out
}

func equalStrSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFts3ExactTermGiveUpPastCorruption is the exact mined statement
// fts3corrupt4.test 41.2: MATCH names the term "ner", which does not exist
// anywhere in 41.1's segdir -- it sorts between real terms "memsys5" and
// "nocase" -- and the SAME node also has a genuinely corrupt term much later
// (declared nDoclist=180 bytes with only 165 remaining). This asserts fix 3:
// the lazy per-segment walk must give up on "ner" the moment it passes
// "nocase" without a match, never reaching that later corruption, matching
// C fts3's own per-token reader (fts3_write.c:2790-2801/:2798).
func TestFts3ExactTermGiveUpPastCorruption(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t1 USING fts3(a,b,c)`,
		`INSERT INTO t1_segdir VALUES(0,0,0,0,'0 835',X'000130120106000106000106001f030001030001030000083230313630363039090107000107000107000001340901050001050001050000013509010400010400010400010730303030303030091c0400010400010400000662696e6172793c0301020200030102020003010202000301020200030102020003010202000301020200030102020003010202000301020200030102020003010202000008636f3870696c657209010200010200010200000664627374617409070300010300010300010465627567090402000102000102000006656e61626c653f07020001020001020001020001020001020001020001020001020001030001010002020001020001020001020001120001020001020001020001020001020001087874656e73696f6e091f0400010400010400000466747334090a0300010300010400030135090d03000103000103000003676363090103000103000103000106656f706f6c790910030001030001030000056a736f6e310913030001030001030000046c6f6164091f030001030001030000036d6178091c02000102000102000105656d6f7279091c03000103000103000304737973350916030001030001030000066e6f636173653c02010202000301020200030102020003010202000301020200030102020003010202000301020200030102020003010202000301020200030102020000046f6d6974091f020001020001020000057274726565091903000103000103000302696d3c010102020003010202000301020200030102020003010202000301020200030102020003010202000301020200030102020003010202000301020200000a746872656164736166650922020001020001020000047674616209070400010400010400000178b401010101020001010102000101010200010101020001010102000101010200010101020001010102000101010200010101020001010102000101010200010101020001010102000101010200010101020001010102000101010200010101020001010102000101010200010101020001010102000101010200010101020001010102000101010200010101020001010102000101010200010101020001010102000101010200')`,
	}
	query := `SELECT offsets(t1) FROM t1 WHERE t1 MATCH 'rtree ner "json1^enable"'`

	got := fts3PoslistBLMSetupAndCompare(t, "fts3_blm_giveup", setup, query)
	// fts3corrupt4.test's own do_execsql_test 41.2 has no third (expected
	// result) argument at all -- the mined pattern this corpus applies to
	// that shape is an empty result set, matching the term "ner" genuinely
	// having zero postings anywhere.
	if len(got) != 0 {
		t.Fatalf("got %d rows, want 0 (term \"ner\" has no postings): %v", len(got), got)
	}
}
