package replication

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samyfodil/musql/engine"
)

// cluster is a set of databases opened with crdt.Open on one in-memory
// network, each under its own site id.
type cluster struct {
	t   *testing.T
	net *MemNetwork
	dir string
	dbs map[string]*sql.DB
}

func newCluster(t *testing.T) *cluster {
	return &cluster{t: t, net: NewMemNetwork(), dir: t.TempDir(), dbs: map[string]*sql.DB{}}
}

// open opens site's database, first running setup on it with a plain
// connection (the schema and rows it has before it is ever synced).
func (c *cluster) open(site string, setup ...string) *sql.DB {
	c.t.Helper()
	return c.openWith(site, nil, setup...)
}

// openWith is open with more options.
func (c *cluster) openWith(site string, opts []Option, setup ...string) *sql.DB {
	c.t.Helper()
	p := filepath.Join(c.dir, site+".db")
	createSchema(c.t, p, setup...)
	opts = append([]Option{CRDT(), WithSite(site), memSync(c.net), WithSyncInterval(20 * time.Millisecond)}, opts...)
	db, err := Open(context.Background(), p, opts...)
	if err != nil {
		c.t.Fatalf("open %s: %v", site, err)
	}
	c.t.Cleanup(func() { db.Close() })
	c.dbs[site] = db
	return db
}

func (c *cluster) exec(site string, stmts ...string) {
	c.t.Helper()
	for _, s := range stmts {
		if _, err := c.dbs[site].Exec(s); err != nil {
			c.t.Fatalf("%s: %s: %v", site, s, err)
		}
	}
}

// partition cuts (or heals) every pair.
func (c *cluster) partition(cut bool) {
	for a := range c.dbs {
		for b := range c.dbs {
			if a < b {
				c.net.Partition(PeerID(a), PeerID(b), cut)
			}
		}
	}
}

// converge waits until every database answers each query identically and
// returns the answers; want, when given, is what the first query must answer.
func (c *cluster) converge(want string, queries ...string) []string {
	c.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var got []string
		same := true
		for _, q := range queries {
			first := ""
			for i, site := range c.sites() {
				d, err := dumpErr(c.dbs[site], q)
				if err != nil {
					d = "error: " + err.Error()
				}
				if i == 0 {
					first = d
				} else if d != first {
					same = false
				}
			}
			got = append(got, first)
		}
		if same && (want == "" || got[0] == want) {
			return got
		}
		if time.Now().After(deadline) {
			var b strings.Builder
			for _, site := range c.sites() {
				for _, q := range queries {
					d, err := dumpErr(c.dbs[site], q)
					b.WriteString(site + ": " + q + " -> " + d)
					if err != nil {
						b.WriteString(" error: " + err.Error())
					}
					b.WriteString("\n")
				}
			}
			c.t.Fatalf("did not converge (want %q):\n%s", want, b.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *cluster) sites() []string {
	var out []string
	for s := range c.dbs {
		out = append(out, s)
	}
	for i := range out { // tiny insertion sort: stable order for messages
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func dumpErr(db *sql.DB, q string) (string, error) {
	rows, err := db.Query(q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var b strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		for i, v := range vals {
			if i > 0 {
				b.WriteByte('|')
			}
			if x, ok := v.([]byte); ok {
				v = string(x)
			}
			b.WriteString(strings.TrimSpace(strings.ReplaceAll(fmtAny(v), "\n", " ")))
		}
		b.WriteByte('\n')
	}
	return b.String(), rows.Err()
}

func fmtAny(v any) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprint(v)
}

const schemaQ = `SELECT type, name, sql FROM sqlite_master WHERE name NOT LIKE '\_repl\_%' ESCAPE '\' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`

// TestSchemaReachesAnEmptyNode: a node opened on an EMPTY database gets the
// schema and the rows another node had before it was synced (genesis), and
// then every schema change as it happens -- a table, an index, a view, an added
// column, a renamed column, a renamed table -- with rows written through each.
func TestSchemaReachesAnEmptyNode(t *testing.T) {
	c := newCluster(t)
	c.open("a", `CREATE TABLE notes(id TEXT PRIMARY KEY, body TEXT NOT NULL)`,
		`INSERT INTO notes VALUES('n0', 'before sync')`,
		`CREATE TABLE log(msg TEXT)`, `INSERT INTO log VALUES('pre')`)
	c.open("b")
	c.converge("n0|before sync\n", `SELECT id, body FROM notes ORDER BY id`, `SELECT msg FROM log`, schemaQ)

	c.exec("a",
		`CREATE TABLE tags(id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE INDEX notes_body ON notes(body)`,
		`CREATE VIEW v AS SELECT id FROM notes`,
		`ALTER TABLE notes ADD COLUMN pinned INTEGER DEFAULT 0`,
		`INSERT INTO notes VALUES('n1', 'one', 1)`)
	c.converge("n0|before sync|0\nn1|one|1\n", `SELECT id, body, pinned FROM notes ORDER BY id`, schemaQ)

	c.exec("b",
		`ALTER TABLE notes RENAME COLUMN body TO text`,
		`ALTER TABLE notes RENAME TO memos`,
		`UPDATE memos SET text = 'renamed twice' WHERE id = 'n0'`,
		`INSERT INTO tags(name) VALUES('red')`)
	c.converge("n0|renamed twice\nn1|one\n", `SELECT id, text FROM memos ORDER BY id`, `SELECT name FROM tags`, schemaQ, `SELECT * FROM v ORDER BY id`)
}

// TestSchemaSameMigrationMerges: two nodes that each run the same migration
// while apart -- the app's startup "CREATE TABLE IF NOT EXISTS" and an "ADD
// COLUMN" -- end up with ONE table holding both nodes' rows, not one node's
// table winning and the other's rows orphaned.
func TestSchemaSameMigrationMerges(t *testing.T) {
	c := newCluster(t)
	c.open("a")
	c.open("b")
	c.partition(true)
	for _, site := range []string{"a", "b"} {
		c.exec(site,
			`CREATE TABLE IF NOT EXISTS items(id TEXT PRIMARY KEY, v TEXT)`,
			`ALTER TABLE items ADD COLUMN qty INTEGER`,
			`INSERT INTO items VALUES('from-`+site+`', 'x', 1)`)
	}
	c.partition(false)
	c.converge("from-a|x|1\nfrom-b|x|1\n", `SELECT * FROM items ORDER BY id`, schemaQ)
}

// TestSchemaRenameKeepsIdentity: an update written under a column's old name,
// on a node that had not seen the rename, lands in the renamed column.
func TestSchemaRenameKeepsIdentity(t *testing.T) {
	c := newCluster(t)
	c.open("a", `CREATE TABLE t(id TEXT PRIMARY KEY, x TEXT)`, `INSERT INTO t VALUES('r', 'old')`)
	c.open("b")
	c.converge("r|old\n", `SELECT * FROM t`)
	c.partition(true)
	c.exec("a", `ALTER TABLE t RENAME COLUMN x TO y`)
	c.exec("b", `UPDATE t SET x = 'written as x' WHERE id = 'r'`)
	c.partition(false)
	c.converge("r|written as x\n", `SELECT id, y FROM t`, schemaQ)
}

// TestSchemaDropWins: a drop beats a concurrent write to the dropped table,
// whatever its clock, and a table later created under the same name with the
// same text is a NEW table -- the dropped one's rows stay out of it.
func TestSchemaDropWins(t *testing.T) {
	c := newCluster(t)
	c.open("a", `CREATE TABLE t(id TEXT PRIMARY KEY, v TEXT)`, `INSERT INTO t VALUES('old', 'x')`)
	c.open("b")
	c.converge("old|x\n", `SELECT * FROM t`)
	c.partition(true)
	c.exec("a", `DROP TABLE t`)
	c.exec("b", `INSERT INTO t VALUES('late', 'y')`) // after the drop, by the clock
	c.partition(false)
	c.converge("", schemaQ)
	for site, db := range c.dbs {
		if _, err := dumpErr(db, `SELECT * FROM t`); err == nil {
			t.Fatalf("%s: t survived its drop", site)
		}
	}
	c.exec("a", `CREATE TABLE t(id TEXT PRIMARY KEY, v TEXT)`, `INSERT INTO t VALUES('new', 'z')`)
	c.converge("new|z\n", `SELECT * FROM t ORDER BY id`)
}

// TestSchemaLateCreateRebuilds: two nodes create DIFFERENT tables under one
// name while apart. The earlier create wins the name everywhere -- including on
// the node that created the later one, which must rebuild its table from the
// winner's definition and rows when the earlier op arrives late.
func TestSchemaLateCreateRebuilds(t *testing.T) {
	c := newCluster(t)
	c.open("a")
	c.open("b")
	c.partition(true)
	c.exec("a", `CREATE TABLE x(id TEXT PRIMARY KEY, a TEXT)`, `INSERT INTO x VALUES('from-a', 'A')`)
	time.Sleep(5 * time.Millisecond) // b's create is later by the clock
	c.exec("b", `CREATE TABLE x(id TEXT PRIMARY KEY, b TEXT)`, `INSERT INTO x VALUES('from-b', 'B')`)
	c.partition(false)
	c.converge("from-a|A\n", `SELECT * FROM x`, schemaQ)
}

// TestSchemaTxnMixesDDLAndRows: one transaction that creates a table, writes
// it, alters it and writes the new column reaches a peer whole.
func TestSchemaTxnMixesDDLAndRows(t *testing.T) {
	c := newCluster(t)
	a := c.open("a")
	c.open("b")
	c.converge("2\n", `SELECT count(*) FROM _repl_oplog WHERE op = 3`)
	// A commit landing while an explicit transaction is open -- a peer's op, an
	// ack, a prune -- fails it with SQLITE_BUSY, as on any database another
	// connection writes; the caller runs it again.
	for attempt := 0; ; attempt++ {
		err := func() error {
			tx, err := a.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback()
			for _, s := range []string{
				`CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)`,
				`INSERT INTO t(v) VALUES('one')`,
				`ALTER TABLE t ADD COLUMN w TEXT`,
				`UPDATE t SET w = 'two'`,
				`INSERT INTO t(v, w) VALUES('three', 'four')`,
			} {
				if _, err := tx.Exec(s); err != nil {
					return fmt.Errorf("%s: %w", s, err)
				}
			}
			return tx.Commit()
		}()
		if err == nil {
			break
		}
		if !errors.Is(err, engine.ErrBusy) || attempt == 20 {
			t.Fatal(err)
		}
	}
	c.converge("one|two\nthree|four\n", `SELECT v, w FROM t ORDER BY v`, schemaQ)
}

// TestSchemaDeclines: what cannot be replicated at all is refused at the
// statement's commit -- and refused whole: the object does not exist afterwards
// on the node that tried.
func TestSchemaDeclines(t *testing.T) {
	c := newCluster(t)
	a := c.open("a", `CREATE TABLE t(id TEXT PRIMARY KEY, v TEXT)`)
	for _, s := range []string{
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END`,
		`CREATE TABLE w(id TEXT PRIMARY KEY, v TEXT) WITHOUT ROWID`,
		`CREATE TABLE k(id TEXT PRIMARY KEY COLLATE NOCASE, v TEXT)`,
		`CREATE VIRTUAL TABLE s USING fts4(body)`,
	} {
		_, err := a.Exec(s)
		if !errors.Is(err, errSchemaUnsupported) {
			t.Errorf("%s: %v, want a refusal", s, err)
		}
	}
	if got, _ := dumpErr(a, `SELECT name FROM sqlite_master WHERE name NOT LIKE '\_repl\_%' ESCAPE '\' AND name NOT LIKE 'sqlite\_%' ESCAPE '\'`); got != "t\n" {
		t.Fatalf("after the refusals the catalog is %q, want only t", got)
	}
}

// TestOpenRefusesUnsupportedSchema: the same list is refused at Open for a
// database that already has it, naming the object.
func TestOpenRefusesUnsupportedSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "u.db")
	createSchema(t, p, `CREATE TABLE t(id TEXT PRIMARY KEY, v TEXT) WITHOUT ROWID`)
	if _, err := Open(context.Background(), p, CRDT()); err == nil || !strings.Contains(err.Error(), "WITHOUT ROWID") {
		t.Fatalf("Open: %v, want the WITHOUT ROWID refusal", err)
	}
}

// TestGenesisKeylessRowsDoNotCollide: two nodes that each had rows in a table
// with no declared key before they were synced keep all of them: each node's
// rows are renumbered into its own rowid range first.
func TestGenesisKeylessRowsDoNotCollide(t *testing.T) {
	c := newCluster(t)
	c.open("a", `CREATE TABLE log(msg TEXT)`, `INSERT INTO log VALUES('a1'), ('a2')`)
	c.open("b", `CREATE TABLE log(msg TEXT)`, `INSERT INTO log VALUES('b1')`)
	c.converge("a1\na2\nb1\n", `SELECT msg FROM log ORDER BY msg`)
}

// TestSchemaTempDDLIsLocal: TEMP objects are the connection's own -- created,
// used and dropped across transactions on a synced database without being
// refused and without reaching a peer.
func TestSchemaTempDDLIsLocal(t *testing.T) {
	c := newCluster(t)
	a := c.open("a", `CREATE TABLE t(id TEXT PRIMARY KEY, v TEXT)`)
	c.open("b")
	conn, err := a.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, s := range []string{
		`CREATE TEMP TABLE scratch(x)`,
		`INSERT INTO scratch VALUES(1)`,
		`CREATE TEMP VIEW tv AS SELECT * FROM t`,
		`DROP VIEW tv`,
		`DROP TABLE scratch`,
		`INSERT INTO t VALUES('r', 'x')`,
	} {
		if _, err := conn.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	c.converge("r|x\n", `SELECT * FROM t`, schemaQ)
}

// TestSchemaDrops: dropping a column, an index and a view replicates, and a
// write to the dropped column made on a node that had not seen the drop stays
// out of the table.
func TestSchemaDrops(t *testing.T) {
	c := newCluster(t)
	c.open("a", `CREATE TABLE t(id TEXT PRIMARY KEY, keep TEXT, gone TEXT)`,
		`CREATE INDEX t_keep ON t(keep)`, `CREATE VIEW v AS SELECT id FROM t`,
		`INSERT INTO t VALUES('r', 'k', 'g')`)
	c.open("b")
	c.converge("r|k|g\n", `SELECT * FROM t`)
	c.partition(true)
	c.exec("a", `ALTER TABLE t DROP COLUMN gone`, `DROP INDEX t_keep`, `DROP VIEW v`)
	c.exec("b", `UPDATE t SET gone = 'late', keep = 'k2' WHERE id = 'r'`)
	c.partition(false)
	c.converge("r|k2\n", `SELECT * FROM t`, schemaQ)
}

// TestSchemaCTAS: CREATE TABLE ... AS SELECT replicates as a table and its
// rows, the rows taking the creating node's rowid range.
func TestSchemaCTAS(t *testing.T) {
	c := newCluster(t)
	c.open("a", `CREATE TABLE src(id TEXT PRIMARY KEY, v TEXT)`, `INSERT INTO src VALUES('1', 'x'), ('2', 'y')`)
	c.open("b")
	c.converge("1|x\n2|y\n", `SELECT * FROM src ORDER BY id`)
	c.partition(true)
	c.exec("a", `CREATE TABLE copy AS SELECT v FROM src`)
	c.exec("b", `CREATE TABLE copy AS SELECT v FROM src`)
	c.partition(false)
	// The same statement on both: the same table, and both nodes' rows.
	c.converge("x\nx\ny\ny\n", `SELECT v FROM copy ORDER BY v`, schemaQ)
}

// TestCompositeKeySyncs: a table keyed on two columns -- declared in the other
// order -- syncs like any other: rows that share one key column stay apart, the
// same key written on two nodes is one row, and moving or deleting a row moves
// or deletes it everywhere.
func TestCompositeKeySyncs(t *testing.T) {
	c := newCluster(t)
	c.open("a", `CREATE TABLE m(a TEXT, b INTEGER, v TEXT, PRIMARY KEY(b, a))`,
		`INSERT INTO m VALUES('x', 1, 'a1'), ('x', 2, 'a2')`)
	c.open("b")
	const q = `SELECT a, b, v FROM m ORDER BY a, b`
	c.converge("x|1|a1\nx|2|a2\n", q)

	c.partition(true)
	c.exec("a", `UPDATE m SET v = 'a-up' WHERE a = 'x' AND b = 1`, `INSERT INTO m VALUES('k', 9, 'from a')`)
	c.exec("b", `INSERT INTO m VALUES('y', 1, 'b1'), ('x', 3, 'b3')`,
		`UPDATE m SET a = 'z' WHERE a = 'x' AND b = 2`, `INSERT INTO m VALUES('k', 9, 'from b')`)
	c.partition(false)
	got := c.converge("", q)[0]
	if got != "k|9|from a\nx|1|a-up\nx|3|b3\ny|1|b1\nz|2|a2\n" && got != "k|9|from b\nx|1|a-up\nx|3|b3\ny|1|b1\nz|2|a2\n" {
		t.Fatalf("converged to %q", got)
	}
	c.exec("a", `DELETE FROM m WHERE a = 'x' AND b = 3`)
	c.converge(strings.Replace(got, "x|3|b3\n", "", 1), q)
}
