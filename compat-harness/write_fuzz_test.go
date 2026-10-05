// Differential fuzzer for the write path. Drives CREATE TABLE, INSERT,
// UPDATE, DELETE, and CREATE INDEX with random statement sequences against
// both the Go engine and C SQLite, comparing final state.
package compat

import (
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// wfDefaultN/wfShortN/wfStmtsPerSeed bound this test's runtime: a normal run
// exercises several hundred seeds (COMPAT_WRITEFUZZ_N overrides), each with a
// modest DML statement budget, so the whole committed test finishes in well
// under a minute.
const (
	wfDefaultN     = 300
	wfShortN       = 30
	wfStmtsPerSeed = 40
	wfIDPoolSize   = 12 // small pool of candidate rowid/unique-column values -> frequent, deliberate collisions
)

// wfColumn is a generated column.
type wfColumn struct {
	name     string
	typ      string
	isIPK    bool
	isUnique bool
}

type wfTable struct {
	name string
	cols []wfColumn
}

type wfSchema struct {
	tables []wfTable
}

// wfStmt is one generated statement.
type wfStmt struct {
	sql   string
	table string
}

var wfColTypes = []string{"INTEGER", "TEXT", "REAL", "BLOB", "NUMERIC"}

// genWFSchema generates random schema with tables and constraints.
func genWFSchema(rng *rand.Rand) (wfSchema, []wfStmt) {
	var schema wfSchema
	var stmts []wfStmt
	idxCounter := 0

	numTables := 1 + rng.Intn(3) // 1-3
	for ti := 0; ti < numTables; ti++ {
		tname := fmt.Sprintf("wf%d", ti)
		numCols := 2 + rng.Intn(4) // 2-5
		cols := make([]wfColumn, numCols)
		for ci := range cols {
			cols[ci] = wfColumn{name: fmt.Sprintf("c%d", ci), typ: wfColTypes[rng.Intn(len(wfColTypes))]}
		}

		mode := rng.Intn(4) // 0 none, 1 single-column INTEGER PK (rowid alias), 2 table PK, 3 table UNIQUE
		colDefs := make([]string, 0, numCols)
		ipkAssigned := false
		for ci := range cols {
			def := cols[ci].name + " " + cols[ci].typ
			switch {
			case mode == 1 && !ipkAssigned && cols[ci].typ == "INTEGER":
				def += " PRIMARY KEY"
				cols[ci].isIPK = true
				ipkAssigned = true
			case rng.Float64() < 0.18:
				def += " UNIQUE"
				cols[ci].isUnique = true
			}
			colDefs = append(colDefs, def)
		}

		tableSuffix := ""
		if mode == 2 || mode == 3 {
			n := 2
			if numCols > 2 && rng.Float64() < 0.4 {
				n = 3
			}
			if n > numCols {
				n = numCols
			}
			perm := rng.Perm(numCols)[:n]
			names := make([]string, n)
			for i, idx := range perm {
				names[i] = cols[idx].name
				cols[idx].isUnique = true
			}
			kw := "PRIMARY KEY"
			if mode == 3 {
				kw = "UNIQUE"
			}
			tableSuffix = fmt.Sprintf(", %s(%s)", kw, strings.Join(names, ","))
		}

		createSQL := fmt.Sprintf("CREATE TABLE %s (%s%s)", tname, strings.Join(colDefs, ", "), tableSuffix)
		stmts = append(stmts, wfStmt{sql: createSQL, table: tname})
		schema.tables = append(schema.tables, wfTable{name: tname, cols: cols})

		numInitIdx := rng.Intn(3) // 0-2
		for k := 0; k < numInitIdx; k++ {
			nc := 1
			if numCols > 1 && rng.Float64() < 0.4 {
				nc = 2
			}
			perm := rng.Perm(numCols)[:nc]
			names := make([]string, nc)
			for i, idx := range perm {
				names[i] = cols[idx].name
			}
			unique := ""
			if rng.Float64() < 0.5 {
				unique = "UNIQUE "
			}
			idxCounter++
			idxName := fmt.Sprintf("idx_%s_%d", tname, idxCounter)
			stmts = append(stmts, wfStmt{
				sql:   fmt.Sprintf("CREATE %sINDEX %s ON %s(%s)", unique, idxName, tname, strings.Join(names, ",")),
				table: tname,
			})
		}
	}
	return schema, stmts
}

// fuzz_test.go's genIntLiteral/genFloatLiteral/genTextLiteral/genBlobLiteral
// (same storage-class-spanning spirit: zero, signed zero, exact int64
// extremes, unicode/empty text, small blobs) but are kept separate because
// this file's INSERT VALUES are restricted to insert_write.go's
// parseValueLiteral grammar (a literal, not a general expression) and
// deliberately never emit the exact MaxInt64 rowid value (see the package
// doc comment's "rowid-random-on-int64-overflow" note) -- fuzz_test.go's
// version does emit MaxInt64 because it is read-only (SELECT) and never
// feeds it back into a later auto-assigned INSERT the way this write fuzzer
// would.
func wfPoolInt(rng *rand.Rand) int64 { return int64(1 + rng.Intn(wfIDPoolSize)) }

func wfGenIntLiteral(rng *rand.Rand) string {
	switch rng.Intn(6) {
	case 0:
		return "0"
	case 1:
		return "9223372036854775806" // MaxInt64-1
	case 2:
		return "-9223372036854775808" // MinInt64
	case 3:
		return "-1"
	case 4:
		return strconv.FormatInt(wfPoolInt(rng), 10)
	default:
		return strconv.FormatInt(rng.Int63n(2_000_000)-1_000_000, 10)
	}
}

func wfIPKIntLiteral(rng *rand.Rand) string {
	switch rng.Intn(4) {
	case 0:
		return "0"
	case 1:
		return "-1"
	case 2:
		return strconv.FormatInt(wfPoolInt(rng), 10)
	default:
		return strconv.FormatInt(rng.Int63n(2_000_000)-1_000_000, 10)
	}
}

func wfGenFloatLiteral(rng *rand.Rand) string {
	switch rng.Intn(5) {
	case 0:
		return "0.0"
	case 1:
		return "-0.0"
	case 2:
		return "1e308"
	case 3:
		return "1e-300"
	default:
		f := (rng.Float64() - 0.5) * pow10(rng.Intn(8))
		digits := 3 + rng.Intn(7)
		rounded, err := strconv.ParseFloat(strconv.FormatFloat(f, 'g', digits, 64), 64)
		if err != nil {
			rounded = f
		}
		return wvFloatLiteral(rounded)
	}
}

var wfWords = []string{"hello", "world", "", "SQLite", "Ω", "日本語", "emoji😀", "O'Brien", "line\nbreak", "  spaced  "}

func wfGenTextLiteral(rng *rand.Rand) string {
	w := wfWords[rng.Intn(len(wfWords))]
	if rng.Float64() < 0.3 {
		w += strconv.FormatInt(wfPoolInt(rng), 10)
	}
	return wvString(w)
}

func wfGenBlobLiteral(rng *rand.Rand) string {
	n := rng.Intn(9) // 0-8 bytes
	b := make([]byte, n)
	rng.Read(b)
	return wvBlob(b)
}

// independent of any column's declared type (cross-type/affinity coverage).
func genWFLiteral(rng *rand.Rand) string {
	switch rng.Intn(5) {
	case 0:
		return "NULL"
	case 1:
		return wfGenIntLiteral(rng)
	case 2:
		return wfGenFloatLiteral(rng)
	case 3:
		return wfGenTextLiteral(rng)
	default:
		return wfGenBlobLiteral(rng)
	}
}

// nowhere near the int64 boundary) in place of wfGenIntLiteral's
// occasional near-boundary extremes -- see wfLiteralForColumn's UNIQUE
// numeric-column case for why this specific substitution matters here.
func genWFLiteralSafeNumeric(rng *rand.Rand) string {
	switch rng.Intn(5) {
	case 0:
		return "NULL"
	case 1:
		return wfIPKIntLiteral(rng)
	case 2:
		return wfGenFloatLiteral(rng)
	case 3:
		return wfGenTextLiteral(rng)
	default:
		return wfGenBlobLiteral(rng)
	}
}

// column draws from a small pool of candidate rowid values (or NULL, for
// auto-assignment) far more often than chance would produce, and a
// UNIQUE-constrained column draws from an even smaller, type-flavored pool --
// both deliberately engineered to make UNIQUE/PK collisions (and hence the
// constraint-parity check) a common, not incidental, occurrence. ANY
// INTEGER/REAL/NUMERIC column -- unique or not -- otherwise falls back to
// near-int64-boundary literal compared (in a WHERE clause) or arithmetic'd
// against a column of numeric affinity can silently lose precision crossing
// through float64 (this package's own sql_eval.go documents this as a known,
// out-of-scope limitation of the shared expression evaluator) and produce a
// WRONG row selection or false UNIQUE-constraint collision that has nothing
// to do with the write path itself -- verified directly: this is what
// caught a plain, non-UNIQUE "WHERE c4 = 9223372036854775806" matching a row
// C SQLite's more careful int/real comparison correctly does NOT match.
// TEXT/BLOB columns aren't affected (comparison there is exact byte
// comparison, never routed through float64) and keep genWFLiteral's full,
// unrestricted variety.
func wfLiteralForColumn(rng *rand.Rand, col wfColumn) string {
	if col.isIPK {
		switch {
		case rng.Float64() < 0.35:
			return "NULL" // auto-assign, exactly like an omitted rowid
		case rng.Float64() < 0.7:
			return strconv.FormatInt(wfPoolInt(rng), 10) // likely collision with an earlier row
		default:
			return wfIPKIntLiteral(rng)
		}
	}
	if col.isUnique && rng.Float64() < 0.5 {
		switch col.typ {
		case "TEXT":
			return wvString(fmt.Sprintf("u%d", wfPoolInt(rng)%5))
		case "REAL":
			return wvFloatLiteral(float64(wfPoolInt(rng) % 5))
		default:
			return strconv.FormatInt(wfPoolInt(rng)%5, 10)
		}
	}
	switch col.typ {
	case "INTEGER", "REAL", "NUMERIC":
		return genWFLiteralSafeNumeric(rng)
	}
	return genWFLiteral(rng)
}

func wfColList(cols []wfColumn) string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.name
	}
	return strings.Join(names, ",")
}

// matches insertStmt's "no explicit column list" path in insert_write.go.
func genWFInsert(rng *rand.Rand, tbl wfTable) string {
	vals := make([]string, len(tbl.cols))
	for i, c := range tbl.cols {
		vals[i] = wfLiteralForColumn(rng, c)
	}
	return fmt.Sprintf("INSERT INTO %s VALUES(%s)", tbl.name, strings.Join(vals, ","))
}

// deliberately restricted to forms sql_eval.go's checkExprSupported always
// accepts (bare comparisons and IS NULL over a column and a literal), since
// this fuzzer's target is the write path's row bookkeeping, not the
// expression evaluator's own function/operator coverage (already exercised
// by fuzz_test.go's randExpr against SELECT).
func genWFPredicate(rng *rand.Rand, c wfColumn) string {
	switch rng.Intn(6) {
	case 0:
		return fmt.Sprintf("%s IS NULL", c.name)
	case 1:
		return fmt.Sprintf("%s IS NOT NULL", c.name)
	case 2:
		return fmt.Sprintf("%s = %s", c.name, wfLiteralForColumn(rng, c))
	case 3:
		return fmt.Sprintf("%s != %s", c.name, wfLiteralForColumn(rng, c))
	case 4:
		return fmt.Sprintf("%s > %s", c.name, wfLiteralForColumn(rng, c))
	default:
		return fmt.Sprintf("%s < %s", c.name, wfLiteralForColumn(rng, c))
	}
}

// predicates joined by AND/OR.
func genWFWhere(rng *rand.Rand, cols []wfColumn) string {
	if rng.Float64() < 0.15 {
		return ""
	}
	n := 1
	if len(cols) > 1 && rng.Float64() < 0.4 {
		n = 2
	}
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = genWFPredicate(rng, cols[rng.Intn(len(cols))])
	}
	joiner := " AND "
	if rng.Float64() < 0.3 {
		joiner = " OR "
	}
	return strings.Join(parts, joiner)
}

// genWFUpdate sets 1-2 columns (a plain new value, or -- for a numeric,
// non-IPK column -- "col + <int literal>", exercising write_update_delete.go's
// "every SET RHS sees the OLD row" semantics) under a random WHERE.
func genWFUpdate(rng *rand.Rand, tbl wfTable) string {
	nSet := 1
	if len(tbl.cols) > 1 && rng.Float64() < 0.4 {
		nSet = 2
	}
	perm := rng.Perm(len(tbl.cols))[:nSet]
	sets := make([]string, len(perm))
	for i, idx := range perm {
		c := tbl.cols[idx]
		rhs := wfLiteralForColumn(rng, c)
		// !c.isUnique: a "col = col + delta" SET, applied to every matched
		// row of a multi-row UPDATE, shifts each row's value relative to
		// its OWN old value -- whether that transiently collides with an
		// UNTOUCHED, not-yet-processed row's OLD value (a genuine real-
		// SQLite UNIQUE-constraint failure -- see write_update_delete.go's
		// Update, which now checks incrementally row-by-row for exactly
		// this) depends on the ORDER rows are processed in. For a plain
		if !c.isIPK && !c.isUnique && (c.typ == "INTEGER" || c.typ == "REAL" || c.typ == "NUMERIC") && rng.Float64() < 0.3 {
			rhs = fmt.Sprintf("%s + %s", c.name, wfIPKIntLiteral(rng))
		}
		sets[i] = fmt.Sprintf("%s = %s", c.name, rhs)
	}
	q := fmt.Sprintf("UPDATE %s SET %s", tbl.name, strings.Join(sets, ", "))
	if where := genWFWhere(rng, tbl.cols); where != "" {
		q += " WHERE " + where
	}
	return q
}

func genWFDelete(rng *rand.Rand, tbl wfTable) string {
	q := "DELETE FROM " + tbl.name
	if where := genWFWhere(rng, tbl.cols); where != "" {
		q += " WHERE " + where
	}
	return q
}

// genWFMidIndex builds a mid-sequence CREATE INDEX that may hit unique violations.
func genWFMidIndex(rng *rand.Rand, tbl wfTable, idxCounter *int) string {
	nc := 1
	if len(tbl.cols) > 1 && rng.Float64() < 0.4 {
		nc = 2
	}
	if nc > len(tbl.cols) {
		nc = len(tbl.cols)
	}
	perm := rng.Perm(len(tbl.cols))[:nc]
	names := make([]string, nc)
	for i, idx := range perm {
		names[i] = tbl.cols[idx].name
	}
	unique := ""
	if rng.Float64() < 0.5 {
		unique = "UNIQUE "
	}
	*idxCounter++
	idxName := fmt.Sprintf("idx_%s_m%d", tbl.name, *idxCounter)
	return fmt.Sprintf("CREATE %sINDEX %s ON %s(%s)", unique, idxName, tbl.name, strings.Join(names, ","))
}

// genWFDML generates a random DML sequence.
func genWFDML(rng *rand.Rand, schema wfSchema, idxCounter *int, n int) []wfStmt {
	out := make([]wfStmt, 0, n)
	for i := 0; i < n; i++ {
		tbl := schema.tables[rng.Intn(len(schema.tables))]
		r := rng.Float64()
		switch {
		case r < 0.50:
			out = append(out, wfStmt{sql: genWFInsert(rng, tbl), table: tbl.name})
		case r < 0.75:
			out = append(out, wfStmt{sql: genWFUpdate(rng, tbl), table: tbl.name})
		case r < 0.90:
			out = append(out, wfStmt{sql: genWFDelete(rng, tbl), table: tbl.name})
		default:
			out = append(out, wfStmt{sql: genWFMidIndex(rng, tbl, idxCounter), table: tbl.name})
		}
	}
	return out
}

// wfClassify buckets an Exec error into "", "constraint", or "unsupported".
func wfClassify(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if strings.Contains(msg, "not supported") || strings.Contains(msg, "unsupported") {
		return "unsupported"
	}
	return "constraint"
}

// dumpWFStmts renders a statement sequence for a divergence repro log.
func dumpWFStmts(stmts []wfStmt) string {
	var sb strings.Builder
	for i, s := range stmts {
		fmt.Fprintf(&sb, "  [%d] %s;\n", i, s.sql)
	}
	return sb.String()
}

// wfSchemaRow is one sqlite_schema row, excluding rootpage.
type wfSchemaRow struct {
	typ, name, tblName string
	sqlText            sql.NullString
}

func wfFetchSchemaRows(t *testing.T, db *sql.DB) []wfSchemaRow {
	t.Helper()
	rows, err := db.Query("SELECT type, name, tbl_name, sql FROM sqlite_schema ORDER BY type, name")
	if err != nil {
		t.Fatalf("sqlite_schema query: %v", err)
	}
	defer rows.Close()
	var out []wfSchemaRow
	for rows.Next() {
		var r wfSchemaRow
		if err := rows.Scan(&r.typ, &r.name, &r.tblName, &r.sqlText); err != nil {
			t.Fatalf("scan sqlite_schema: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// wfCompareSchema requires the Go-written file and the C-SQLite-written file
// to have IDENTICAL sqlite_schema rows (type, name, tbl_name, sql -- every
// table and every explicit AND automatic index), in the same order (both are
// built from the identical statement sequence, so a legitimate run produces
// identical schema-object ordering, including automatic index numbering --
// see index_write.go's buildAutoIndexes doc comment).
func wfCompareSchema(t *testing.T, seed int64, goSQL, cgoSQL *sql.DB, allStmts []wfStmt) {
	t.Helper()
	got := wfFetchSchemaRows(t, goSQL)
	want := wfFetchSchemaRows(t, cgoSQL)
	if len(got) != len(want) {
		t.Fatalf("DIVERGENCE: sqlite_schema row count Go=%d C=%d (seed %d)\n  got:  %+v\n  want: %+v\nfull sequence:\n%s",
			len(got), len(want), seed, got, want, dumpWFStmts(allStmts))
	}
	for i := range got {
		g, w := got[i], want[i]
		if g.typ != w.typ || g.name != w.name || g.tblName != w.tblName || g.sqlText != w.sqlText {
			t.Fatalf("DIVERGENCE: sqlite_schema row %d mismatch (seed %d)\n  Go: %+v\n  C:  %+v\nfull sequence:\n%s",
				i, seed, g, w, dumpWFStmts(allStmts))
		}
	}
}

// runWriteFuzzSeed generates and applies exactly one seed's schema+DML
// sequence in lockstep against the Go engine writer and C SQLite (see
// the package doc comment above for the full protocol), then compares final
// state: PRAGMA integrity_check on the Go-written file, every live table's
// rows (ascending rowid), and sqlite_schema.
func runWriteFuzzSeed(t *testing.T, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	schema, schemaStmts := genWFSchema(rng)
	idxCounter := 1000 // disjoint from genWFSchema's own counter (starts at 0), so index names never collide
	dmlStmts := genWFDML(rng, schema, &idxCounter, wfStmtsPerSeed)
	allStmts := append(schemaStmts, dmlStmts...)

	goPath := filepath.Join(t.TempDir(), "go.sqlite")
	cgoPath := filepath.Join(t.TempDir(), "cgo.sqlite")

	godb, err := engine.Create(goPath)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	cgodb, err := sql.Open("sqlite3", cgoPath)
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer cgodb.Close()
	if _, err := cgodb.Exec("PRAGMA synchronous=OFF"); err != nil {
		t.Fatalf("cgo PRAGMA synchronous=OFF: %v", err)
	}

	liveTable := make(map[string]bool, len(schema.tables))
	for _, tbl := range schema.tables {
		liveTable[tbl.name] = true
	}

	var applied, constraintAgreed, skippedUnsupported int
	for i, st := range allStmts {
		if !liveTable[st.table] {
			continue // this table was never created on either side; keep skipping it in lockstep
		}
		goErr := godb.Exec(st.sql)
		switch wfClassify(goErr) {
		case "":
			applied++
			if _, cerr := cgodb.Exec(st.sql); cerr != nil {
				t.Fatalf("DIVERGENCE (Go accepted, C SQLite rejected) at statement #%d\n  seed: %d\n  stmt: %s\n  C error: %v\nfull sequence:\n%s",
					i, seed, st.sql, cerr, dumpWFStmts(allStmts))
			}
		case "constraint":
			if _, cerr := cgodb.Exec(st.sql); cerr == nil {
				t.Fatalf("DIVERGENCE (Go rejected as a constraint violation, C SQLite ACCEPTED) at statement #%d\n  seed: %d\n  stmt: %s\n  Go error: %v\nfull sequence:\n%s",
					i, seed, st.sql, goErr, dumpWFStmts(allStmts))
			}
			constraintAgreed++
		case "unsupported":
			skippedUnsupported++
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(st.sql)), "CREATE TABLE") {
				liveTable[st.table] = false
				t.Logf("seed %d: statement #%d (CREATE TABLE %s) unexpectedly reported unsupported, disabling that table for the rest of the sequence: %v",
					seed, i, st.table, goErr)
			}
		}
	}

	if err := godb.Close(); err != nil {
		t.Fatalf("engine writer Close: %v (seed %d)\nfull sequence:\n%s", err, seed, dumpWFStmts(allStmts))
	}

	goSQL := openCSQLite(t, goPath)
	requireIntegrityOK(t, goSQL)

	for _, tbl := range schema.tables {
		if !liveTable[tbl.name] {
			continue
		}
		colList := wfColList(tbl.cols)
		gotRows := fetchRowidRows(t, goSQL, tbl.name, colList)
		wantRows := fetchRowidRows(t, cgodb, tbl.name, colList)
		requireRowsMatch(t, tbl.name, gotRows, wantRows)
	}
	wfCompareSchema(t, seed, goSQL, cgodb, allStmts)

	if t.Failed() {
		t.Logf("seed %d FAILED -- full statement sequence:\n%s", seed, dumpWFStmts(allStmts))
	}

	t.Logf("seed %d: statements=%d applied=%d constraintRejectedBothSides=%d skippedUnsupported=%d",
		seed, len(allStmts), applied, constraintAgreed, skippedUnsupported)
}

// TestWriteFuzz is the write path's differential fuzzer.
func TestWriteFuzz(t *testing.T) {
	n := wfDefaultN
	if testing.Short() {
		n = wfShortN
	}
	if v := os.Getenv("COMPAT_WRITEFUZZ_N"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	t.Logf("write-fuzz: seeds=0..%d, ~%d statements/seed (COMPAT_WRITEFUZZ_N to change; deterministic/reproducible)", n-1, wfStmtsPerSeed)
	for seed := 0; seed < n; seed++ {
		seed := int64(seed)
		t.Run(fmt.Sprintf("seed%04d", seed), func(t *testing.T) {
			runWriteFuzzSeed(t, seed)
		})
	}
}
