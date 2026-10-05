// Tests identifier folding in various contexts: planner, foreign keys,
// attached databases, windows, compounds, and triggers.
package compat

import (
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// TestR33SIdentFoldPlanner tests identifier folding through index operations.
func TestR33SIdentFoldPlanner(t *testing.T) {
	for i, p := range r33sPairs {
		qa, qb := r33sQuote(p.a), r33sQuote(p.b)
		differ(t, fmt.Sprintf("r33s planner pair#%d(%s)", i, p.why), []string{
			fmt.Sprintf(`CREATE TABLE pl(%s INTEGER, %s INTEGER, pad TEXT)`, qa, qb),
			fmt.Sprintf(`CREATE INDEX pla ON pl(%s)`, qa),
			fmt.Sprintf(`CREATE INDEX plb ON pl(%s DESC)`, qb),
			`INSERT INTO pl VALUES(1,90,'a'),(2,80,'b'),(3,70,'c'),(2,60,'d'),(NULL,50,'e')`,
			fmt.Sprintf(`SELECT * FROM pl WHERE %s = 2 ORDER BY pad`, qa),
			fmt.Sprintf(`SELECT * FROM pl WHERE %s = 2 ORDER BY pad`, qb),
			fmt.Sprintf(`SELECT * FROM pl WHERE %s > 1 AND %s < 90 ORDER BY pad`, qa, qb),
			fmt.Sprintf(`SELECT %s, %s FROM pl ORDER BY %s`, qa, qb, qa),
			fmt.Sprintf(`SELECT %s, %s FROM pl ORDER BY %s DESC`, qa, qb, qb),
			fmt.Sprintf(`SELECT count(%s), count(%s), min(%s), max(%s) FROM pl`, qa, qb, qa, qb),
			fmt.Sprintf(`SELECT %s FROM pl INDEXED BY pla WHERE %s IS NOT NULL ORDER BY 1`, qb, qa),
			fmt.Sprintf(`SELECT %s FROM pl INDEXED BY plb WHERE %s > 0 ORDER BY 1`, qa, qb),
			fmt.Sprintf(`SELECT %s, count(*) FROM pl GROUP BY %s ORDER BY 1`, qa, qa),
			fmt.Sprintf(`SELECT DISTINCT %s FROM pl ORDER BY 1`, qb),
			`PRAGMA index_list(pl)`,
			`ANALYZE`,
			`SELECT tbl, idx, stat FROM sqlite_stat1 ORDER BY idx`,
			`DROP INDEX plb`,
			`SELECT name FROM sqlite_master WHERE type='index' ORDER BY name`,
		})
	}
}

// TestR33SIdentFoldForeignKey tests identifier folding in foreign key references.
func TestR33SIdentFoldForeignKey(t *testing.T) {
	for i, p := range r33sPairs {
		qa, qb := r33sQuote(p.a), r33sQuote(p.b)
		differ(t, fmt.Sprintf("r33s fk pair#%d(%s)", i, p.why), []string{
			`PRAGMA foreign_keys=ON`,
			fmt.Sprintf(`CREATE TABLE par(%s INTEGER UNIQUE, %s INTEGER UNIQUE)`, qa, qb),
			fmt.Sprintf(`CREATE TABLE kid(v INTEGER REFERENCES par(%s))`, qb),
			`INSERT INTO par VALUES(1,2)`,
			`INSERT INTO kid VALUES(2)`,
			`INSERT INTO kid VALUES(1)`,
			`SELECT * FROM kid ORDER BY rowid`,
			`PRAGMA foreign_key_list(kid)`,
			`PRAGMA foreign_key_check`,
		})
	}
}

// TestR33SIdentFoldAttachAlias tests identifier folding in ATTACH aliases.
func TestR33SIdentFoldAttachAlias(t *testing.T) {
	for i, p := range r33sPairs {
		qa, qb := r33sQuote(p.a), r33sQuote(p.b)
		differ(t, fmt.Sprintf("r33s attach pair#%d(%s)", i, p.why), []string{
			fmt.Sprintf(`ATTACH ':memory:' AS %s`, qa),
			fmt.Sprintf(`ATTACH ':memory:' AS %s`, qb),
			fmt.Sprintf(`CREATE TABLE %s.t(x)`, qa),
			fmt.Sprintf(`INSERT INTO %s.t VALUES(1)`, qa),
			fmt.Sprintf(`SELECT * FROM %s.t`, qa),
			fmt.Sprintf(`SELECT * FROM %s.t`, qb),
			fmt.Sprintf(`DETACH %s`, qb),
			fmt.Sprintf(`SELECT * FROM %s.t`, qa),
		})
	}
}

// TestR33SIdentFoldWindowAndCompound tests identifier folding in windows and compound selects.
func TestR33SIdentFoldWindowAndCompound(t *testing.T) {
	for i, p := range r33sPairs {
		qa, qb := r33sQuote(p.a), r33sQuote(p.b)
		differ(t, fmt.Sprintf("r33s window pair#%d(%s)", i, p.why), []string{
			fmt.Sprintf(`CREATE TABLE ww(%s INTEGER, %s INTEGER)`, qa, qb),
			`INSERT INTO ww VALUES(1,10),(1,20),(2,30)`,
			fmt.Sprintf(`SELECT %s, sum(%s) OVER (PARTITION BY %s ORDER BY %s) FROM ww ORDER BY 1,2`, qa, qb, qa, qb),
			fmt.Sprintf(`SELECT %s, row_number() OVER w FROM ww WINDOW w AS (PARTITION BY %s ORDER BY %s) ORDER BY 1,2`, qb, qa, qb),
			fmt.Sprintf(`SELECT %s AS %s, count(*) OVER () FROM ww ORDER BY 1`, qb, qa),
		})
		differ(t, fmt.Sprintf("r33s compound pair#%d(%s)", i, p.why), []string{
			fmt.Sprintf(`CREATE TABLE c1(%s INTEGER, %s INTEGER)`, qa, qb),
			fmt.Sprintf(`CREATE TABLE c2(%s INTEGER, %s INTEGER)`, qb, qa),
			`INSERT INTO c1 VALUES(1,2)`,
			`INSERT INTO c2 VALUES(3,4)`,
			fmt.Sprintf(`SELECT %s, %s FROM c1 UNION ALL SELECT %s, %s FROM c2 ORDER BY 1`, qa, qb, qa, qb),
			`SELECT * FROM c1 UNION ALL SELECT * FROM c2 ORDER BY 1`,
			fmt.Sprintf(`SELECT %s FROM c1 UNION SELECT %s FROM c2 ORDER BY %s`, qa, qb, qa),
			fmt.Sprintf(`SELECT %s FROM c1 EXCEPT SELECT %s FROM c2`, qa, qa),
			fmt.Sprintf(`SELECT %s FROM c1 INTERSECT SELECT %s FROM c2`, qb, qb),
		})
	}
}

// TestR33SIdentFoldViewTrigger: an INSTEAD OF trigger on a view whose two
// columns differ only by a non-ASCII case distinction -- the NEW. reference
// inside the body and the view's own column list are both resolved by name.
func TestR33SIdentFoldViewTrigger(t *testing.T) {
	for i, p := range r33sPairs {
		qa, qb := r33sQuote(p.a), r33sQuote(p.b)
		// The pair goes on the VIEW's column list, not the base table.
		differ(t, fmt.Sprintf("r33s viewtrig pair#%d(%s)", i, p.why), []string{
			`CREATE TABLE base(x INTEGER, y INTEGER)`,
			fmt.Sprintf(`CREATE VIEW vv(%s,%s) AS SELECT x, y FROM base`, qa, qb),
			fmt.Sprintf(`CREATE TRIGGER vt INSTEAD OF INSERT ON vv BEGIN INSERT INTO base VALUES(NEW.%s, NEW.%s); END`, qb, qa),
			`INSERT INTO vv VALUES(1,2)`,
			`SELECT * FROM base`,
			`PRAGMA table_info(vv)`,
			`DROP VIEW vv`,
			`SELECT name FROM sqlite_master WHERE type IN ('view','trigger') ORDER BY name`,
		})
	}
}
