// Differential tests for rtree/rtree_i32 virtual tables against C SQLite,
// comparing result sets and behaviors for spatial queries, constraints, and mutations.
package compat

import (
	"database/sql"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// rtreeCounters accumulates the fuzz outcome for the summary report.
type rtreeCounters struct {
	execTotal, execMatched, execDeclined, execWrong int
	queryTotal, queryMatched, queryWrong            int
}

// execBothCount runs a query on both engines and compares results: both succeeding
// compares RowsAffected; both erroring or both succeeding counts as a match; a
// success/error split is a divergence.
func (c *rtreeCounters) execBothCount(t *testing.T, pure, mattn *sql.DB, query string, checkRA bool) {
	t.Helper()
	c.execTotal++
	pRes, pErr := pure.Exec(query)
	mRes, mErr := mattn.Exec(query)
	if (pErr == nil) != (mErr == nil) {
		c.execWrong++
		t.Errorf("EXEC error divergence:\n  sql:   %s\n  pure:  %v\n  mattn: %v", query, pErr, mErr)
		return
	}
	if pErr != nil {
		c.execDeclined++ // both rejected identically (constraint / bad box)
		return
	}
	if checkRA {
		pRA, _ := pRes.RowsAffected()
		mRA, _ := mRes.RowsAffected()
		if pRA != mRA {
			c.execWrong++
			t.Errorf("EXEC RowsAffected divergence:\n  sql: %s\n  pure=%d mattn=%d", query, pRA, mRA)
			return
		}
	}
	c.execMatched++
}

// queryBothCount runs query on both engines and compares normalized rows.
func (c *rtreeCounters) queryBothCount(t *testing.T, pure, mattn *sql.DB, query string) {
	t.Helper()
	c.queryTotal++
	pRows, pErr := pure.Query(query)
	mRows, mErr := mattn.Query(query)
	if (pErr == nil) != (mErr == nil) {
		c.queryWrong++
		t.Errorf("QUERY error divergence:\n  sql:   %s\n  pure:  %v\n  mattn: %v", query, pErr, mErr)
		return
	}
	if pErr != nil {
		c.queryMatched++
		return
	}
	defer pRows.Close()
	defer mRows.Close()
	pc, po := collectRows(t, pRows)
	mc, mo := collectRows(t, mRows)
	if ok, reason := queryResultsMatch(pc, po, mc, mo, true); !ok {
		c.queryWrong++
		t.Errorf("QUERY diverges:\n  sql:   %s\n  reason:%s\n  pure:  %v\n  mattn: %v", query, reason, po, mo)
		return
	}
	c.queryMatched++
}

// fmtCoord formats a float64 to the shortest round-trippable form.
func fmtCoord(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

// TestRtreeDiffFuzz fuzzes random rtree/rtree_i32 tables through random
// INSERT/UPDATE/DELETE sequences and compares dumps, spatial queries, counts,
// and constraint rejections against mattn's real rtree.
func TestRtreeDiffFuzz(t *testing.T) {
	pure, mattn, _, _ := openPair(t, "rtreefuzz")
	rng := rand.New(rand.NewSource(0x271EE))
	var c rtreeCounters

	const iters = 120
	for iter := 0; iter < iters; iter++ {
		name := fmt.Sprintf("rt%d", iter)
		dims := rng.Intn(3) + 1 // 1..3 dimensions
		i32 := rng.Intn(4) == 0
		variant := "rtree"
		if i32 {
			variant = "rtree_i32"
		}

		colDefs := "id"
		coordNames := make([]string, 0, dims*2)
		for d := 0; d < dims; d++ {
			lo := fmt.Sprintf("min%d", d)
			hi := fmt.Sprintf("max%d", d)
			colDefs += ", " + lo + ", " + hi
			coordNames = append(coordNames, lo, hi)
		}
		c.execBothCount(t, pure, mattn, fmt.Sprintf("CREATE VIRTUAL TABLE %s USING %s(%s)", name, variant, colDefs), false)

		usedIDs := map[int]bool{}
		mkCoord := func(min bool) string {
			if i32 {
				return strconv.Itoa(rng.Intn(4000000000) - 1500000000) // spans int32 overflow
			}
			return fmtCoord(rng.Float64()*200 - 100)
		}
		// a coordinate pair, usually valid (lo<=hi) but occasionally inverted.
		mkPair := func() (string, string) {
			a := mkCoord(true)
			b := mkCoord(false)
			if rng.Intn(6) == 0 {
				return b, a // possibly inverted -> both engines must reject the same way
			}
			fa, _ := strconv.ParseFloat(a, 64)
			fb, _ := strconv.ParseFloat(b, 64)
			if fa > fb {
				a, b = b, a
			}
			return a, b
		}

		// ---- inserts ----
		nInsert := rng.Intn(8) + 2
		for k := 0; k < nInsert; k++ {
			var idPart string
			switch rng.Intn(3) {
			case 0:
				idPart = "NULL" // auto-assign; LastInsertId must match
			default:
				id := rng.Intn(40) + 1
				idPart = strconv.Itoa(id)
				usedIDs[id] = true // may collide -> both must reject identically
			}
			vals := idPart
			for d := 0; d < dims; d++ {
				lo, hi := mkPair()
				vals += ", " + lo + ", " + hi
			}
			c.execBothCount(t, pure, mattn, fmt.Sprintf("INSERT INTO %s VALUES(%s)", name, vals), true)
		}

		proj := "id"
		for _, cn := range coordNames {
			proj += ", " + cn
		}
		c.queryBothCount(t, pure, mattn, fmt.Sprintf("SELECT %s FROM %s ORDER BY id", proj, name))
		c.queryBothCount(t, pure, mattn, fmt.Sprintf("SELECT count(*) FROM %s", name))

		// ---- spatial queries ----
		for q := 0; q < 4; q++ {
			b1 := rng.Float64()*200 - 100
			b2 := b1 + rng.Float64()*80
			c.queryBothCount(t, pure, mattn, fmt.Sprintf(
				"SELECT %s FROM %s WHERE min0>=%s AND max0<=%s ORDER BY id",
				proj, name, fmtCoord(b1), fmtCoord(b2)))
		}
		c.queryBothCount(t, pure, mattn, fmt.Sprintf(
			"SELECT id FROM %s WHERE max0>=%s ORDER BY id DESC", name, fmtCoord(rng.Float64()*100-50)))

		// ---- updates ----
		for u := 0; u < rng.Intn(4); u++ {
			id := rng.Intn(40) + 1
			lo, hi := mkPair()
			c.execBothCount(t, pure, mattn, fmt.Sprintf(
				"UPDATE %s SET min0=%s, max0=%s WHERE id=%d", name, lo, hi, id), true)
		}
		// occasionally reassign the id (rowid) column
		if rng.Intn(2) == 0 {
			from := rng.Intn(40) + 1
			to := rng.Intn(80) + 41
			c.execBothCount(t, pure, mattn, fmt.Sprintf("UPDATE %s SET id=%d WHERE id=%d", name, to, from), true)
		}
		c.queryBothCount(t, pure, mattn, fmt.Sprintf("SELECT %s FROM %s ORDER BY id", proj, name))

		// ---- deletes ----
		for d := 0; d < rng.Intn(3); d++ {
			id := rng.Intn(80) + 1
			c.execBothCount(t, pure, mattn, fmt.Sprintf("DELETE FROM %s WHERE id=%d", name, id), true)
		}
		c.execBothCount(t, pure, mattn, fmt.Sprintf("DELETE FROM %s WHERE min0>=%s", name, fmtCoord(rng.Float64()*100-50)), true)
		c.queryBothCount(t, pure, mattn, fmt.Sprintf("SELECT %s FROM %s ORDER BY id", proj, name))
	}

	t.Logf("RTREE-FUZZ SUMMARY: exec total=%d matched=%d declined=%d wrong=%d | query total=%d matched=%d wrong=%d",
		c.execTotal, c.execMatched, c.execDeclined, c.execWrong,
		c.queryTotal, c.queryMatched, c.queryWrong)
	if c.execWrong != 0 || c.queryWrong != 0 {
		t.Fatalf("rtree fuzz found %d exec + %d query divergences from mattn", c.execWrong, c.queryWrong)
	}
}

// TestVtabInsertSelect checks INSERT INTO virtual table SELECT, including
// self-insert cases where the table must read as it was before the statement.
func TestVtabInsertSelect(t *testing.T) {
	pure, mattn, _, _ := openPair(t, "vtabinsertselect")
	var c rtreeCounters

	setup := []string{
		`CREATE TABLE src(id, x0, x1)`,
		`INSERT INTO src VALUES(1, 0.0, 1.0), (2, 2.0, 3.0), (3, 4.0, 5.0)`,
		`CREATE VIRTUAL TABLE r USING rtree(id, min0, max0)`,
	}
	for _, s := range setup {
		c.execBothCount(t, pure, mattn, s, false)
	}
	for _, s := range []string{
		`INSERT INTO r SELECT id, x0, x1 FROM src WHERE id<3`,
		// An explicit, reordered target column list.
		`INSERT INTO r(max0, min0, id) SELECT x1, x0, id+10 FROM src WHERE id=3`,
		// A source SELECT carrying WHERE / ORDER BY / LIMIT.
		`INSERT INTO r SELECT id+20, x0, x1 FROM src ORDER BY id DESC LIMIT 2`,
		// Reading the target itself.
		`INSERT INTO r SELECT id+100, min0, max0 FROM r`,
		// A source that selects nothing at all.
		`INSERT INTO r SELECT id+900, x0, x1 FROM src WHERE 0`,
	} {
		c.execBothCount(t, pure, mattn, s, true)
		c.queryBothCount(t, pure, mattn, `SELECT id, min0, max0 FROM r ORDER BY id`)
	}
	// A column-count mismatch must be REJECTED by both, not silently padded.
	c.execBothCount(t, pure, mattn, `INSERT INTO r SELECT id, x0 FROM src`, false)
	c.queryBothCount(t, pure, mattn, `SELECT id, min0, max0 FROM r ORDER BY id`)

	if c.execWrong != 0 || c.queryWrong != 0 {
		t.Fatalf("INSERT ... SELECT into a virtual table: %d exec + %d query divergences from mattn", c.execWrong, c.queryWrong)
	}
}

// TestVtabModuleArgumentsByToken checks module argument parsing: arguments are
// cut by token, comments between arguments are ignored, quoted names are dequoted,
// and keywords in column names cause syntax errors.
func TestVtabModuleArgumentsByToken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"comments between rtree arguments", []string{
			"CREATE VIRTUAL TABLE demo_index USING rtree(\n  id,            -- Integer primary key\n  minX, maxX,    -- Minimum and maximum X coordinate\n  minY, maxY     /* Minimum and maximum Y coordinate */\n)",
			"CREATE TABLE demo_data(id INTEGER PRIMARY KEY, objname TEXT)",
			"INSERT INTO demo_index VALUES(1, 0, 1, 2, 3)",
			"INSERT INTO demo_data VALUES(1, 'one')",
			"SELECT * FROM demo_index",
			"SELECT * FROM demo_index NATURAL JOIN demo_data",
			"SELECT minX, maxY FROM demo_index WHERE minX <= 0.5",
		}},
		{"a keyword where a column name must be", []string{
			"CREATE VIRTUAL TABLE t7 USING rtree(index, x1, y1, x2, y2)",
			"SELECT count(*) FROM sqlite_master WHERE name='t7'",
		}},
		{"quoted names, a type after the name, and an empty argument", []string{
			`CREATE VIRTUAL TABLE r2 USING rtree(id INTEGER, "min x" FLOAT, [max x] ,, "min y", "max y")`,
			"INSERT INTO r2 VALUES(7, 1, 2, 3, 4)",
			"SELECT * FROM r2",
			`SELECT "min x", [max x] FROM r2`,
		}},
		{"fts4 column list with comments", []string{
			"CREATE VIRTUAL TABLE f USING fts4(\n  a,  -- the first\n  b   /* the second */\n)",
			"INSERT INTO f VALUES('hello world', 'second col')",
			"SELECT * FROM f",
			"SELECT a FROM f WHERE f MATCH 'hello'",
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "vtab module arguments: "+tc.name, tc.stmts) })
	}
}

// TestRtreeTreeMatchesC validates rtree tree maintenance through inserts, deletes,
// and updates, checking that shadow tables and scan order match C SQLite exactly.
func TestRtreeTreeMatchesC(t *testing.T) {
	for _, cfg := range []struct {
		module string
		dims   int
		seed   int64
		n      int // rows inserted
	}{
		{"rtree", 1, 1, 400}, {"rtree", 2, 2, 400}, {"rtree", 3, 3, 400}, {"rtree_i32", 2, 4, 400}, {"rtree", 2, 5, 400},
		{"rtree", 5, 6, 400}, {"rtree_i32", 1, 7, 400}, {"rtree", 1, 8, 3000}, {"rtree_i32", 3, 9, 1500},
	} {
		cfg := cfg
		t.Run(fmt.Sprintf("%s_%dd_seed%d", cfg.module, cfg.dims, cfg.seed), func(t *testing.T) {
			if cfg.n > 400 && testing.Short() {
				// Deep tree is slow with many single-row statements.
				t.Skip("deep-tree configuration: full run only")
			}
			r := rand.New(rand.NewSource(cfg.seed))
			cols := "id"
			for d := 0; d < cfg.dims; d++ {
				cols += fmt.Sprintf(", lo%d, hi%d", d, d)
			}
			coords := func() string {
				s := ""
				for d := 0; d < cfg.dims; d++ {
					lo := r.Float64()*1000 - 500
					if cfg.module == "rtree_i32" {
						lo = float64(int(lo))
					}
					s += fmt.Sprintf(", %v, %v", lo, lo+r.Float64()*50)
				}
				return s
			}
			stmts := []string{fmt.Sprintf("CREATE VIRTUAL TABLE rt USING %s(%s)", cfg.module, cols)}
			var ids []int
			for i := 0; i < cfg.n; i++ {
				if r.Intn(5) == 0 {
					stmts = append(stmts, "INSERT INTO rt VALUES(NULL"+coords()+")")
					continue
				}
				id := r.Intn(cfg.n*12) + 1
				ids = append(ids, id)
				stmts = append(stmts, fmt.Sprintf("INSERT OR IGNORE INTO rt VALUES(%d%s)", id, coords()))
			}
			for i := 0; i < cfg.n*5/8; i++ {
				stmts = append(stmts, fmt.Sprintf("DELETE FROM rt WHERE id = %d", ids[r.Intn(len(ids))]))
			}
			stmts = append(stmts, "DELETE FROM rt WHERE id % 7 = 0")
			for i := 0; i < 40; i++ {
				id := ids[r.Intn(len(ids))]
				switch r.Intn(3) {
				case 0:
					stmts = append(stmts, fmt.Sprintf("UPDATE rt SET lo0 = lo0 - 1 WHERE id = %d", id))
				case 1:
					stmts = append(stmts, fmt.Sprintf("UPDATE OR IGNORE rt SET id = %d WHERE id = %d", cfg.n*20+i, id))
				default:
					stmts = append(stmts, fmt.Sprintf("UPDATE rt SET id = NULL WHERE id = %d", id))
				}
			}
			stmts = append(stmts,
				"INSERT INTO rt SELECT NULL"+strings.TrimPrefix(strings.ReplaceAll(cols, "id, ", ", "), "id")+" FROM rt WHERE id < 800",
				"SELECT nodeno, hex(data) FROM rt_node ORDER BY nodeno",
				"SELECT nodeno, parentnode FROM rt_parent ORDER BY nodeno",
				"SELECT rowid, nodeno FROM rt_rowid ORDER BY rowid",
				"SELECT id FROM rt",
				"SELECT id FROM rt WHERE lo0 < 0",
				"SELECT count(*) FROM rt_node",
			)
			differ(t, "rtree tree shape: "+t.Name(), stmts)
		})
	}
}
