// Pins a decline: ATTACH with a subquery path is accepted by C SQLite 3.53.3
// but declined here. The parser lacks a connection snapshot to evaluate the subquery.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

func TestI4AttachSubqueryOverATableStillDeclined(t *testing.T) {
	dir := t.TempDir()
	// ATTACH's path expression evaluates to a VALUE; a relative one would land
	// in the process's working directory, which is the source tree. AGENTS.md
	// invariant 4 forbids that.
	t.Chdir(dir)
	target := filepath.Join(dir, "attach-subquery.db")

	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
	if err != nil {
		t.Fatal(err)
	}
	cgodb.SetMaxOpenConns(1) // ATTACH is connection-scoped
	defer cgodb.Close()
	for _, s := range []string{"CREATE TABLE t(p)", fmt.Sprintf("INSERT INTO t VALUES(%q)", target)} {
		if _, err := cgodb.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cgodb.Exec("ATTACH (SELECT p FROM t) AS y"); err != nil {
		t.Errorf("the oracle now REJECTS the disclosed shape (%v) -- group I4 recorded it as ACCEPTED; "+
			"re-read engine/attach.go's parseAttachPath before trusting that disclosure again", err)
	}

	if _, _, ok, err := engine.ParseAttachStmt("ATTACH (SELECT p FROM t) AS y"); err == nil && ok {
		t.Errorf("the engine now ACCEPTS the disclosed shape -- that is the FIX landing, and it needs a real " +
			"parity gate (same file attached, writes reach it) rather than this decline pin")
	}
}
