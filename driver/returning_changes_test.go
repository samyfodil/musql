package driver_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// TestAbortedReturningPublishesChanges verifies that changes() reflects the
// correct count after an aborted RETURNING statement.
func TestAbortedReturningPublishesChanges(t *testing.T) {
	for _, inTxn := range []bool{false, true} {
		name := "autocommit"
		if inTxn {
			name = "in a transaction"
		}
		t.Run(name, func(t *testing.T) {
			db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "c.db"))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer db.Close()
			// One connection throughout: changes() is connection state.
			conn, err := db.Conn(t.Context())
			if err != nil {
				t.Fatalf("conn: %v", err)
			}
			defer conn.Close()
			for _, s := range []string{
				`CREATE TABLE u(a UNIQUE, b NOT NULL)`,
				`INSERT INTO u VALUES(1,'x')`,
				`INSERT INTO u VALUES(9,'z')`,
			} {
				if _, err := conn.ExecContext(t.Context(), s); err != nil {
					t.Fatalf("%s: %v", s, err)
				}
			}
			if inTxn {
				if _, err := conn.ExecContext(t.Context(), `BEGIN`); err != nil {
					t.Fatalf("BEGIN: %v", err)
				}
			}
			var changes int64
			if err := conn.QueryRowContext(t.Context(), `SELECT changes()`).Scan(&changes); err != nil {
				t.Fatalf("changes(): %v", err)
			}
			if changes != 1 {
				t.Fatalf("changes() before the aborted statement = %d, want 1", changes)
			}
			// Aborts on u.b's NOT NULL, after the conflict resolution picked
			// the DO UPDATE arm.
			const stmt = `INSERT INTO u VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET b=NULL RETURNING a,b`
			rows, qerr := conn.QueryContext(t.Context(), stmt)
			if qerr == nil {
				qerr = rows.Err()
				for rows.Next() {
				}
				if e := rows.Err(); e != nil {
					qerr = e
				}
				rows.Close()
			}
			if qerr == nil {
				t.Fatalf("%s SUCCEEDED; it violates u.b's NOT NULL", stmt)
			}
			if err := conn.QueryRowContext(t.Context(), `SELECT changes()`).Scan(&changes); err != nil {
				t.Fatalf("changes() after: %v", err)
			}
			if changes != 0 {
				t.Errorf("changes() after an ABORTED RETURNING statement = %d, want 0 "+
					"(the oracle's answer; the statement changed nothing)", changes)
			}
			var n int
			if err := conn.QueryRowContext(t.Context(), `SELECT count(*) FROM u WHERE b IS NULL`).Scan(&n); err != nil {
				t.Fatalf("count: %v", err)
			}
			if n != 0 {
				t.Errorf("the aborted statement stored %d NULL rows, want 0", n)
			}
			if inTxn {
				if _, err := conn.ExecContext(t.Context(), `ROLLBACK`); err != nil {
					t.Fatalf("ROLLBACK: %v", err)
				}
			}
		})
	}
}
