package compat

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestFuzzDifferential generates random SQL programs (schema + inserts +
// queries) with a fixed seed and runs each through differ() to compare musql
// and C SQLite answers.
const fuzzSeed = 424242

const defaultFuzzN = 500 // ~500 programs; bump via COMPAT_FUZZ_N=<n> (e.g. 2000-5000) for a deeper sweep

func TestFuzzDifferential(t *testing.T) {
	n := defaultFuzzN
	if v := os.Getenv("COMPAT_FUZZ_N"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	t.Logf("fuzz: seed=%d n=%d (COMPAT_FUZZ_N to change; deterministic/reproducible)", fuzzSeed, n)

	rng := rand.New(rand.NewSource(fuzzSeed))
	for i := 0; i < n; i++ {
		stmts := genProgram(rng)
		name := fmt.Sprintf("p%04d", i)
		t.Run(name, func(t *testing.T) {
			if !differ(t, name, stmts) {
				t.Logf("program #%d DIVERGED (seed=%d) -- reproduce with: go test -run 'TestFuzzDifferential/%s' -count=1 -v", i, fuzzSeed, name)
			}
		})
	}
}

// ---- random SQL program generation -----------------------------------

var fuzzColumnTypes = []string{"INTEGER", "REAL", "TEXT", "BLOB", "NUMERIC", ""}

var fuzzCastTypes = []string{"INTEGER", "REAL", "TEXT", "BLOB", "NUMERIC"}

var fuzzWords = []string{
	"hello", "world", "", "SQLite", "Ω", "日本語", "emoji😀test",
	"O'Brien", "line\nbreak", "tab\ttab", "  spaced  ", "NULL-ish", "'pre-quoted'",
	"x", "abcabcabc", "MiXeDcAsE",
}

// genProgram builds one self-contained SQL program: 1-3 tables with random
// columns/constraints, random inserts spanning the type space (including
// cross-type values to exercise affinity), and a handful of random queries.
func genProgram(rng *rand.Rand) []string {
	var stmts []string

	type tableInfo struct {
		name string
		cols []string
	}
	numTables := 1 + rng.Intn(3)
	tables := make([]tableInfo, 0, numTables)

	for ti := 0; ti < numTables; ti++ {
		tname := fmt.Sprintf("t%d", ti)
		numCols := 2 + rng.Intn(4) // 2-5
		colDefs := make([]string, 0, numCols)
		colNames := make([]string, 0, numCols)
		usedPK := false
		for ci := 0; ci < numCols; ci++ {
			cname := fmt.Sprintf("c%d", ci)
			colNames = append(colNames, cname)

			typ := fuzzColumnTypes[rng.Intn(len(fuzzColumnTypes))]
			def := cname
			if typ != "" {
				def += " " + typ
			}
			switch {
			case !usedPK && rng.Float64() < 0.25:
				def += " PRIMARY KEY"
				// AUTOINCREMENT ensures deterministic rowid assignment,
				// avoiding SQLite's random fallback on int64 overflow.
				if typ == "INTEGER" {
					def += " AUTOINCREMENT"
				}
				usedPK = true
			case rng.Float64() < 0.15:
				def += " UNIQUE"
			}
			if rng.Float64() < 0.2 {
				def += " NOT NULL"
			}
			if rng.Float64() < 0.15 {
				def += " DEFAULT " + genLiteral(rng)
			}
			colDefs = append(colDefs, def)
		}
		stmts = append(stmts, fmt.Sprintf("CREATE TABLE %s(%s)", tname, strings.Join(colDefs, ", ")))
		tables = append(tables, tableInfo{tname, colNames})

		numRows := 3 + rng.Intn(6) // 3-8
		for ri := 0; ri < numRows; ri++ {
			vals := make([]string, numCols)
			for ci := range vals {
				vals[ci] = genLiteral(rng)
			}
			stmts = append(stmts, fmt.Sprintf("INSERT INTO %s VALUES(%s)", tname, strings.Join(vals, ", ")))
		}
	}

	numQueries := 2 + rng.Intn(3) // 2-4
	for qi := 0; qi < numQueries; qi++ {
		tbl := tables[rng.Intn(len(tables))]
		if rng.Float64() < 0.4 {
			stmts = append(stmts, genAggregateSelect(rng, tbl.name, tbl.cols))
		} else {
			stmts = append(stmts, genPlainSelect(rng, tbl.name, tbl.cols))
		}
	}
	return stmts
}

// genPlainSelect builds a projection/where/order/limit query. ORDER BY always
// covers every source column (never a computed alias) plus a trailing rowid
// tiebreak, so row order is fully deterministic -- any observed difference is
// a genuine engine divergence, never sort-stability or query-plan noise.
func genPlainSelect(rng *rand.Rand, table string, cols []string) string {
	numProj := 1 + rng.Intn(4)
	proj := make([]string, numProj)
	for i := range proj {
		proj[i] = randExpr(rng, cols, 2)
	}
	q := fmt.Sprintf("SELECT %s FROM %s", strings.Join(proj, ", "), table)
	if rng.Float64() < 0.6 {
		q += " WHERE " + randExpr(rng, cols, 2)
	}

	orderParts := make([]string, 0, len(cols)+1)
	for _, c := range cols {
		part := c
		if rng.Float64() < 0.2 {
			part += " COLLATE NOCASE"
		}
		if rng.Float64() < 0.3 {
			part += " DESC"
		}
		orderParts = append(orderParts, part)
	}
	orderParts = append(orderParts, "rowid")
	q += " ORDER BY " + strings.Join(orderParts, ", ")

	if rng.Float64() < 0.5 {
		q += fmt.Sprintf(" LIMIT %d", 1+rng.Intn(10))
	}
	return q
}

// genAggregateSelect builds a GROUP BY query. Projections are restricted to
// the grouping columns plus aggregates (never a bare non-grouped column), and
// ORDER BY covers the grouping columns, so result order is deterministic.
func genAggregateSelect(rng *rand.Rand, table string, cols []string) string {
	groupCols := pickSubset(rng, cols, 1, 2)
	if len(groupCols) == 0 {
		groupCols = cols[:1]
	}
	aggFns := []string{
		"count(*)", "count(%s)", "sum(%s)", "avg(%s)",
		"min(%s)", "max(%s)", "total(%s)", "group_concat(%s,'-')",
	}
	numAgg := 1 + rng.Intn(3)
	aggExprs := make([]string, numAgg)
	for i := range aggExprs {
		fn := aggFns[rng.Intn(len(aggFns))]
		if strings.Contains(fn, "%s") {
			col := cols[rng.Intn(len(cols))]
			aggExprs[i] = fmt.Sprintf(fn, col)
		} else {
			aggExprs[i] = fn
		}
	}
	proj := append(append([]string{}, groupCols...), aggExprs...)
	q := fmt.Sprintf("SELECT %s FROM %s", strings.Join(proj, ", "), table)
	if rng.Float64() < 0.5 {
		q += " WHERE " + randExpr(rng, cols, 1)
	}
	q += " GROUP BY " + strings.Join(groupCols, ", ")
	if rng.Float64() < 0.3 {
		q += fmt.Sprintf(" HAVING count(*) > %d", rng.Intn(2))
	}
	q += " ORDER BY " + strings.Join(groupCols, ", ")
	return q
}

// randExpr builds a bounded-depth scalar expression over cols, mixing
// arithmetic, comparisons, boolean logic, CASE, and the common scalar
// functions named in the task: abs/round/length/substr/upper/lower/coalesce/
// typeof/cast/instr/replace.
func randExpr(rng *rand.Rand, cols []string, depth int) string {
	if depth <= 0 || rng.Float64() < 0.35 {
		if len(cols) > 0 && rng.Float64() < 0.6 {
			return cols[rng.Intn(len(cols))]
		}
		return genLiteral(rng)
	}
	a := func() string { return randExpr(rng, cols, depth-1) }
	switch rng.Intn(14) {
	case 0:
		return fmt.Sprintf("(%s + %s)", a(), a())
	case 1:
		return fmt.Sprintf("(%s - %s)", a(), a())
	case 2:
		return fmt.Sprintf("(%s * %s)", a(), a())
	case 3:
		op := []string{"=", "!=", "<", ">", "<=", ">="}[rng.Intn(6)]
		return fmt.Sprintf("(%s %s %s)", a(), op, a())
	case 4:
		return fmt.Sprintf("(%s AND %s)", a(), a())
	case 5:
		return fmt.Sprintf("(%s OR %s)", a(), a())
	case 6:
		return fmt.Sprintf("NOT (%s)", a())
	case 7:
		return fmt.Sprintf("CASE WHEN %s THEN %s ELSE %s END", a(), a(), a())
	case 8:
		fn := []string{"abs", "length", "upper", "lower", "typeof"}[rng.Intn(5)]
		return fmt.Sprintf("%s(%s)", fn, a())
	case 9:
		if rng.Float64() < 0.5 {
			return fmt.Sprintf("round(%s)", a())
		}
		return fmt.Sprintf("round(%s, %d)", a(), rng.Intn(4))
	case 10:
		return fmt.Sprintf("substr(%s, %d, %d)", a(), rng.Intn(6)-2, rng.Intn(6))
	case 11:
		return fmt.Sprintf("coalesce(%s, %s)", a(), a())
	case 12:
		return fmt.Sprintf("instr(%s, %s)", a(), a())
	case 13:
		return fmt.Sprintf("replace(%s, %s, %s)", a(), a(), a())
	default:
		typ := fuzzCastTypes[rng.Intn(len(fuzzCastTypes))]
		return fmt.Sprintf("CAST(%s AS %s)", a(), typ)
	}
}

func pickSubset(rng *rand.Rand, items []string, min, max int) []string {
	if len(items) == 0 {
		return nil
	}
	n := min + rng.Intn(max-min+1)
	if n > len(items) {
		n = len(items)
	}
	idxs := rng.Perm(len(items))[:n]
	out := make([]string, n)
	for i, idx := range idxs {
		out[i] = items[idx]
	}
	return out
}

// genLiteral produces a SQL literal spanning the full storage-class space:
// NULL, ints (incl. min/max int64), floats (incl. signed zero, very
// large/small), ascii+unicode+empty text, and blobs. The kind is chosen
// independently of any column's declared type, so most inserts exercise
// cross-type affinity coercion as well.
func genLiteral(rng *rand.Rand) string {
	r := rng.Float64()
	switch {
	case r < 0.10:
		return "NULL"
	case r < 0.35:
		return genIntLiteral(rng)
	case r < 0.55:
		return genFloatLiteral(rng)
	case r < 0.85:
		return genTextLiteral(rng)
	default:
		return genBlobLiteral(rng)
	}
}

func genIntLiteral(rng *rand.Rand) string {
	switch rng.Intn(6) {
	case 0:
		return "0"
	case 1:
		return "9223372036854775807" // math.MaxInt64
	case 2:
		return "-9223372036854775808" // math.MinInt64 (parses as REAL in SQLite -- expected)
	case 3:
		return "-1"
	default:
		return strconv.FormatInt(rng.Int63n(2_000_000)-1_000_000, 10)
	}
}

func genFloatLiteral(rng *rand.Rand) string {
	switch rng.Intn(6) {
	case 0:
		return "0.0"
	case 1:
		return "-0.0"
	case 2:
		return "1e308"
	case 3:
		return "-1e308"
	case 4:
		return "1e-300"
	default:
		f := (rng.Float64() - 0.5) * pow10(rng.Intn(10))
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
}

func pow10(n int) float64 {
	v := 1.0
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}

func genTextLiteral(rng *rand.Rand) string {
	w := fuzzWords[rng.Intn(len(fuzzWords))]
	if rng.Float64() < 0.3 {
		w += strconv.Itoa(rng.Intn(1000))
	}
	return "'" + strings.ReplaceAll(w, "'", "''") + "'"
}

func genBlobLiteral(rng *rand.Rand) string {
	n := rng.Intn(9) // 0-8 bytes
	b := make([]byte, n)
	rng.Read(b)
	return fmt.Sprintf("x'%x'", b)
}
