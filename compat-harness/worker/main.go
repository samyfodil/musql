// Command worker executes a JSON array of SQL statements against ONE SQLite
// engine (selected at build time via a build tag) on a single connection, and
// writes a JSON array of normalized per-statement results to stdout. The
// differential harness builds this once per engine and diffs the outputs.
package main

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"unicode/utf8"
)

// stmtResult is the normalized outcome of one statement. Error *text* is
// intentionally omitted from comparison (drivers word errors differently); only
// whether it errored, and the query data, are compat-relevant.
type stmtResult struct {
	Kind string     `json:"kind"`           // "rows" | "ok" | "error"
	Cols []string   `json:"cols,omitempty"` // for Kind=="rows"
	Rows [][]string `json:"rows,omitempty"` // normalized cell values
	Err  string     `json:"err,omitempty"`  // error text, only when COMPAT_ERRTEXT=1
}

// errText is set by the harness for the gates that compare error wording; the
// default differ leaves it off so error text stays out of its comparison.
var errText = os.Getenv("COMPAT_ERRTEXT") == "1"

func errResult(err error) stmtResult {
	if errText {
		return stmtResult{Kind: "error", Err: err.Error()}
	}
	return stmtResult{Kind: "error"}
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: worker <statements.json>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var stmts []string
	if err := json.Unmarshal(raw, &stmts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	// A private on-disk db per run (so file-backed paths, journals and WAL are
	// exercised); the harness passes a fresh temp dir via DSN below.
	dsn := os.Getenv("COMPAT_DSN")
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	db.SetMaxOpenConns(1) // one connection: statements share session/txn state
	defer db.Close()

	results := make([]stmtResult, 0, len(stmts))
	for _, s := range stmts {
		results = append(results, runOne(db, s))
	}
	out, _ := json.Marshal(results)
	os.Stdout.Write(out)
}

func runOne(db *sql.DB, sqlText string) stmtResult {
	rows, err := db.Query(sqlText)
	if err != nil {
		if errText {
			// The gates that compare error wording run each statement exactly
			// once. Re-running a failed one is not harmless: after an ON
			// CONFLICT ROLLBACK the transaction is gone and the second run
			// succeeds, where C SQLite (which reports the error from the rows,
			// not from Query) never runs it twice.
			return errResult(err)
		}
		// Not a query (or a real error). Try Exec to distinguish DDL/DML that
		// returns no rows from an actual error.
		if _, eerr := db.Exec(sqlText); eerr != nil {
			return errResult(err)
		}
		return stmtResult{Kind: "ok"}
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return errResult(err)
	}
	res := stmtResult{Kind: "rows", Cols: cols}
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return errResult(err)
		}
		norm := make([]string, len(cols))
		for i, c := range cells {
			norm[i] = normalize(c)
		}
		res.Rows = append(res.Rows, norm)
	}
	if err := rows.Err(); err != nil {
		return errResult(err)
	}
	return res
}

// normalize renders a scanned value into a canonical, driver-independent string
// tagged by SQLite storage class. TEXT surfaces as string in some drivers and
// []byte in others — both collapse to "T:" when the bytes are valid UTF-8, so
// that driver API differences don't masquerade as compatibility failures; only
// genuinely binary blobs become "X:".
func normalize(v any) string {
	switch x := v.(type) {
	case nil:
		return "N"
	case int64:
		return "I:" + strconv.FormatInt(x, 10)
	case float64:
		return "F:" + strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		if x {
			return "I:1"
		}
		return "I:0"
	case string:
		return "T:" + x
	case []byte:
		if utf8.Valid(x) {
			return "T:" + string(x)
		}
		return "X:" + hex.EncodeToString(x)
	default:
		return "?:" + fmt.Sprint(v)
	}
}
