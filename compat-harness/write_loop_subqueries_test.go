package compat

import "testing"

// TestWriteLoopSubqueriesMatchC gates trigger bodies and upsert DO UPDATE with
// subqueries, including self-referential ones.
func TestWriteLoopSubqueriesMatchC(t *testing.T) {
	body := []string{
		`CREATE TABLE b(id INTEGER PRIMARY KEY, v INTEGER)`,
		`CREATE TABLE c(id INTEGER PRIMARY KEY, n INTEGER)`,
		`INSERT INTO b VALUES(1,1),(2,2),(3,3)`,
		`INSERT INTO c VALUES(1,0),(2,0),(3,0),(4,0),(5,0)`,
		`CREATE TABLE e(x)`,
	}
	bodyCases := map[string][]string{
		"body-delete-halloween":  {`CREATE TRIGGER t0 AFTER INSERT ON e BEGIN DELETE FROM c WHERE EXISTS(SELECT 1 FROM c AS c2 WHERE c2.id = c.id - 1); END`, `INSERT INTO e VALUES(1)`, `SELECT id FROM c ORDER BY id`},
		"body-delete-uncorr":     {`CREATE TRIGGER t0 AFTER INSERT ON e BEGIN DELETE FROM c WHERE id > (SELECT count(*) FROM c) - 3; END`, `INSERT INTO e VALUES(1)`, `SELECT id FROM c ORDER BY id`},
		"body-delete-shortcirc":  {`CREATE TRIGGER t0 AFTER INSERT ON e BEGIN DELETE FROM c WHERE id = 1 OR (SELECT count(*) FROM c) = 5; END`, `INSERT INTO e VALUES(1)`, `SELECT id FROM c ORDER BY id`},
		"body-update-where-self": {`CREATE TRIGGER t0 AFTER INSERT ON e BEGIN UPDATE c SET n = 7 WHERE (SELECT count(*) FROM c AS c2 WHERE c2.n = 0 AND c2.id < c.id) >= 2; END`, `INSERT INTO e VALUES(1)`, `SELECT id, n FROM c ORDER BY id`},
		"body-update-set-self":   {`CREATE TRIGGER t0 AFTER INSERT ON e BEGIN UPDATE c SET n = (SELECT sum(n) FROM c AS c2 WHERE c2.id < c.id) + c.id; END`, `INSERT INTO e VALUES(1)`, `SELECT id, n FROM c ORDER BY id`},
		"body-update-set-trigw":  {`CREATE TRIGGER tr BEFORE UPDATE ON c BEGIN INSERT INTO b VALUES(NULL, 9); END`, `CREATE TRIGGER t0 AFTER INSERT ON e BEGIN UPDATE c SET n = (SELECT count(*) FROM b WHERE b.id >= c.id); END`, `INSERT INTO e VALUES(1)`, `SELECT id, n FROM c ORDER BY id`},
		"body-view-set":          {`CREATE VIEW vc AS SELECT id, n FROM c`, `CREATE TRIGGER tv INSTEAD OF UPDATE ON vc BEGIN INSERT INTO b VALUES(NULL, new.n); END`, `CREATE TRIGGER t0 AFTER INSERT ON e BEGIN UPDATE vc SET n = (SELECT count(*) FROM b WHERE b.id >= vc.id); END`, `INSERT INTO e VALUES(1)`, `SELECT id, v FROM b ORDER BY id`},
		"body-view-where":        {`CREATE VIEW vc AS SELECT id, n FROM c`, `CREATE TRIGGER tv INSTEAD OF DELETE ON vc BEGIN DELETE FROM c WHERE id = old.id; END`, `CREATE TRIGGER t0 AFTER INSERT ON e BEGIN DELETE FROM vc WHERE EXISTS(SELECT 1 FROM c AS c2 WHERE c2.id = vc.id - 1); END`, `INSERT INTO e VALUES(1)`, `SELECT id FROM c ORDER BY id`},
		"worowid-asc":            {`CREATE TABLE w(k INTEGER PRIMARY KEY, n) WITHOUT ROWID`, `INSERT INTO w VALUES(3,0),(1,0),(2,0)`, `CREATE TRIGGER tr BEFORE UPDATE ON w BEGIN INSERT INTO b VALUES(NULL, 9); END`, `UPDATE w SET n = (SELECT count(*) FROM b WHERE b.id >= w.k)`, `SELECT k, n FROM w ORDER BY k`},
		"worowid-desc":           {`CREATE TABLE w(k INTEGER, n, PRIMARY KEY(k DESC)) WITHOUT ROWID`, `INSERT INTO w VALUES(3,0),(1,0),(2,0)`, `CREATE TRIGGER tr BEFORE UPDATE ON w BEGIN INSERT INTO b VALUES(NULL, 9); END`, `UPDATE w SET n = (SELECT count(*) FROM b WHERE b.id >= w.k)`, `SELECT k, n FROM w ORDER BY k`},
		"worowid-text-nocase":    {`CREATE TABLE w(k TEXT COLLATE NOCASE PRIMARY KEY, n) WITHOUT ROWID`, `INSERT INTO w VALUES('b',0),('A',0),('c',0)`, `CREATE TRIGGER tr BEFORE UPDATE ON w BEGIN INSERT INTO b VALUES(NULL, 9); END`, `UPDATE w SET n = (SELECT count(*) FROM b)`, `UPDATE w SET n = (SELECT count(*) FROM b WHERE b.v = 9 AND w.k <> 'zz')`, `SELECT k, n FROM w ORDER BY k`},
		"worowid-self":           {`CREATE TABLE w(k INTEGER PRIMARY KEY, n) WITHOUT ROWID`, `INSERT INTO w VALUES(3,3),(1,1),(2,2)`, `UPDATE w SET n = (SELECT sum(n) FROM w AS u WHERE u.k <= w.k)`, `SELECT k, n FROM w ORDER BY k`},
		"worowid-self-where":     {`CREATE TABLE w(k INTEGER PRIMARY KEY, n) WITHOUT ROWID`, `INSERT INTO w VALUES(3,3),(1,1),(2,2)`, `UPDATE w SET n = (SELECT sum(n) FROM w AS u WHERE u.k <= w.k) WHERE k >= 2`, `SELECT k, n FROM w ORDER BY k`},
	}
	ups := []string{
		`CREATE TABLE b(id INTEGER PRIMARY KEY, v INTEGER)`,
		`CREATE TABLE c(id INTEGER PRIMARY KEY, n INTEGER)`,
		`INSERT INTO b VALUES(1,1),(2,2),(3,3)`,
		`INSERT INTO c VALUES(1,10),(2,20)`,
		`CREATE TABLE src(k, v)`,
		`INSERT INTO src VALUES(1,100),(9,900),(2,200)`,
	}
	upsCases := map[string][]string{
		"set-uncorr":            {`INSERT INTO b VALUES(1,0) ON CONFLICT(id) DO UPDATE SET v = (SELECT max(v) FROM b) + 1`, `SELECT id,v FROM b ORDER BY id`},
		"set-uncorr-multi":      {`INSERT INTO b VALUES(1,0),(4,4),(2,0) ON CONFLICT(id) DO UPDATE SET v = (SELECT max(v) FROM b) + 1`, `SELECT id,v FROM b ORDER BY id`},
		"set-corr-excluded":     {`INSERT INTO b VALUES(1,0),(2,0) ON CONFLICT(id) DO UPDATE SET v = (SELECT sum(v) FROM b AS u WHERE u.id <= excluded.id)`, `SELECT id,v FROM b ORDER BY id`},
		"set-corr-target":       {`INSERT INTO b VALUES(1,0),(2,0) ON CONFLICT(id) DO UPDATE SET v = (SELECT n FROM c WHERE c.id = b.id)`, `SELECT id,v FROM b ORDER BY id`},
		"set-corr-target-bare":  {`INSERT INTO b VALUES(1,0),(3,0) ON CONFLICT(id) DO UPDATE SET v = (SELECT v * 10)`, `SELECT id,v FROM b ORDER BY id`},
		"set-mixed":             {`INSERT INTO b VALUES(1,0),(2,0),(3,0) ON CONFLICT(id) DO UPDATE SET v = (SELECT max(n) FROM c) + (SELECT excluded.id * 1000)`, `SELECT id,v FROM b ORDER BY id`},
		"set-random-once":       {`INSERT INTO b VALUES(1,0),(2,0),(3,0) ON CONFLICT(id) DO UPDATE SET v = (SELECT abs(random()) % 1000000 FROM c LIMIT 1)`, `SELECT count(DISTINCT v) FROM b`},
		"set-sees-earlier":      {`INSERT INTO b VALUES(7,70),(1,0) ON CONFLICT(id) DO UPDATE SET v = (SELECT max(v) FROM b)`, `SELECT id,v FROM b ORDER BY id`},
		"where-uncorr":          {`INSERT INTO b VALUES(1,0) ON CONFLICT(id) DO UPDATE SET v = 7 WHERE EXISTS(SELECT 1 FROM c)`, `SELECT id,v FROM b ORDER BY id`},
		"where-uncorr-false":    {`INSERT INTO b VALUES(1,0) ON CONFLICT(id) DO UPDATE SET v = 7 WHERE EXISTS(SELECT 1 FROM c WHERE n > 99)`, `SELECT id,v FROM b ORDER BY id`},
		"where-corr":            {`INSERT INTO b VALUES(1,0),(2,0),(3,0) ON CONFLICT(id) DO UPDATE SET v = 7 WHERE b.id IN (SELECT id FROM c)`, `SELECT id,v FROM b ORDER BY id`},
		"where-corr-excluded":   {`INSERT INTO b VALUES(1,5),(2,50) ON CONFLICT(id) DO UPDATE SET v = excluded.v WHERE (SELECT count(*) FROM c WHERE n < excluded.v) > 0`, `SELECT id,v FROM b ORDER BY id`},
		"select-source":         {`INSERT INTO b(id,v) SELECT k, v FROM src WHERE true ON CONFLICT(id) DO UPDATE SET v = (SELECT count(*) FROM b) * 1000 + excluded.v`, `SELECT id,v FROM b ORDER BY id`},
		"select-source-corr":    {`INSERT INTO b(id,v) SELECT k, v FROM src WHERE true ON CONFLICT(id) DO UPDATE SET v = (SELECT sum(v) FROM b AS u WHERE u.id < excluded.id)`, `SELECT id,v FROM b ORDER BY id`},
		"named-excluded":        {`CREATE TABLE excluded(a INTEGER PRIMARY KEY, c INT DEFAULT 0)`, `INSERT INTO excluded VALUES(1,5)`, `INSERT INTO excluded(a) VALUES(1) ON CONFLICT(a) DO UPDATE SET c = (SELECT excluded.c + 1)`, `SELECT * FROM excluded`},
		"returning":             {`INSERT INTO b VALUES(1,0),(2,0) ON CONFLICT(id) DO UPDATE SET v = (SELECT max(n) FROM c) RETURNING id, v`},
		"in-trigger":            {`CREATE TABLE e(x)`, `CREATE TRIGGER t0 AFTER INSERT ON e BEGIN INSERT INTO b VALUES(new.x, 0) ON CONFLICT(id) DO UPDATE SET v = (SELECT count(*) FROM c) + new.x; END`, `INSERT INTO e VALUES(1),(2),(8)`, `SELECT id,v FROM b ORDER BY id`},
		"excluded-compound":     {`INSERT INTO b VALUES(1,50),(2,7) ON CONFLICT(id) DO UPDATE SET v = (SELECT excluded.v UNION SELECT 9 ORDER BY 1 LIMIT 1)`, `SELECT id,v FROM b ORDER BY id`},
		"body-new-and-excluded": {`CREATE TABLE k(a INTEGER PRIMARY KEY, c INTEGER)`, `INSERT INTO k VALUES(1,10)`, `CREATE TABLE tt(a INTEGER, c INTEGER)`, `CREATE TRIGGER tg AFTER INSERT ON tt BEGIN INSERT INTO k VALUES(1,5) ON CONFLICT(a) DO UPDATE SET c = (SELECT excluded.c*100 + new.c); END`, `INSERT INTO tt VALUES(1,3),(1,4)`, `SELECT * FROM k`},
		"do-update-trigger":     {`CREATE TABLE lg(x)`, `CREATE TRIGGER tu AFTER UPDATE ON b BEGIN INSERT INTO c VALUES(NULL, new.v); END`, `INSERT INTO b VALUES(1,0),(2,0) ON CONFLICT(id) DO UPDATE SET v = (SELECT count(*) FROM c)`, `SELECT id,v FROM b ORDER BY id`},
	}
	// A ONE-PASS UPDATE (no trigger, no subquery in the WHERE) whose SET reads
	// the target per row: C visits rows in the order where.c chose for the WHERE
	// (sqlite3WhereBegin with WHERE_ONEPASS_DESIRED, update.c:742), which
	// DB.updateOnePassOrder asks the ported planner for.
	onePass := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, k, n)`, `CREATE INDEX tk ON t(k)`,
		`INSERT INTO t VALUES(1,30,1),(2,10,2),(3,20,3),(4,10,4),(5,40,5)`}
	onePassCases := map[string][]string{
		"eq":           {`UPDATE t SET n = (SELECT sum(n) FROM t AS u WHERE u.id <= t.id) WHERE k = 10`, `SELECT * FROM t ORDER BY id`},
		"range":        {`UPDATE t SET n = (SELECT sum(n) FROM t AS u WHERE u.id <= t.id) WHERE k > 15`, `SELECT * FROM t ORDER BY id`},
		"range-k":      {`UPDATE t SET n = (SELECT sum(n) FROM t AS u WHERE u.k <= t.k) WHERE k > 15`, `SELECT * FROM t ORDER BY id`},
		"in":           {`UPDATE t SET n = (SELECT sum(n) FROM t AS u WHERE u.k <= t.k) WHERE k IN (40, 20, 30)`, `SELECT * FROM t ORDER BY id`},
		"no-index":     {`UPDATE t SET n = (SELECT sum(n) FROM t AS u WHERE u.id <= t.id) WHERE n > 1`, `SELECT * FROM t ORDER BY id`},
		"indexed-by":   {`UPDATE t INDEXED BY tk SET n = (SELECT sum(n) FROM t AS u WHERE u.k <= t.k) WHERE k > 0`, `SELECT * FROM t ORDER BY id`},
		"not-indexed":  {`UPDATE t NOT INDEXED SET n = (SELECT sum(n) FROM t AS u WHERE u.k <= t.k) WHERE k > 15`, `SELECT * FROM t ORDER BY id`},
		"covering":     {`UPDATE t SET n = (SELECT sum(n) FROM t AS u WHERE u.k <= t.k) WHERE k + 0 > 15`, `SELECT * FROM t ORDER BY id`},
		"reverse":      {`PRAGMA reverse_unordered_selects=1`, `UPDATE t SET n = (SELECT sum(n) FROM t AS u WHERE u.id <= t.id)`, `SELECT * FROM t ORDER BY id`},
		"reverse-idx":  {`PRAGMA reverse_unordered_selects=1`, `UPDATE t SET n = (SELECT sum(n) FROM t AS u WHERE u.k <= t.k) WHERE k > 15`, `SELECT * FROM t ORDER BY id`},
		"reverse-late": {`UPDATE t SET n = (SELECT sum(n) FROM t AS u WHERE u.id <= t.id)`, `PRAGMA reverse_unordered_selects=1`, `UPDATE t SET n = (SELECT sum(n) FROM t AS u WHERE u.id <= t.id)`, `SELECT * FROM t ORDER BY id`},
	}
	// The pragma through a HELD session: inside a transaction nothing reopens
	// it, so the driver has to push the setter into it (tuningPragma).
	onePassCases["reverse-in-txn"] = []string{`BEGIN`, `PRAGMA reverse_unordered_selects=1`, `UPDATE t SET n = (SELECT sum(n) FROM t AS u WHERE u.id <= t.id)`, `COMMIT`, `SELECT * FROM t ORDER BY id`}
	onePassCases["reverse-late-in-txn"] = []string{`BEGIN`, `UPDATE t SET n = (SELECT sum(n) FROM t AS u WHERE u.id <= t.id)`, `PRAGMA reverse_unordered_selects=1`, `UPDATE t SET n = (SELECT sum(n) FROM t AS u WHERE u.id <= t.id)`, `COMMIT`, `SELECT * FROM t ORDER BY id`}
	onePassCases["reverse-not-indexed"] = []string{`PRAGMA reverse_unordered_selects=1`, `UPDATE t NOT INDEXED SET n = (SELECT sum(n) FROM t AS u WHERE u.k <= t.k) WHERE k > 15`, `SELECT * FROM t ORDER BY id`}
	for name, stmts := range onePassCases {
		differ(t, "onepass/"+name, append(append([]string{}, onePass...), stmts...))
	}
	// ...and a session opened AFTER the setter -- here the one an ON CONFLICT
	// ROLLBACK discards the held session for -- which only the stamp at open
	// (openWriteOrCreatePath) carries.
	differ(t, "onepass/reverse-after-session-discard", append(append([]string{}, onePass...),
		`PRAGMA reverse_unordered_selects=1`, `BEGIN`, `INSERT OR ROLLBACK INTO t VALUES(1, 0, 0)`,
		`UPDATE t SET n = (SELECT sum(n) FROM t AS u WHERE u.id <= t.id)`, `SELECT * FROM t ORDER BY id`))
	// The shapes engine tests used to pin as DECLINES, run with data. Each has
	// an order-sensitive twin (a subquery over the column the loop writes),
	// since a count over an untouched column answers the same in any order.
	promoted := map[string][]string{
		"index-range": {`CREATE TABLE t(a,b)`, `CREATE INDEX tb ON t(b)`, `INSERT INTO t VALUES(1,1),(2,0),(3,5),(4,2),(5,0),(6,3)`,
			`UPDATE t SET b=(SELECT count(*) FROM t t2 WHERE t2.a<=t.a) WHERE b>0`, `SELECT a,b FROM t ORDER BY a`},
		"index-range-sensitive": {`CREATE TABLE t(a,b)`, `CREATE INDEX tb ON t(b)`, `INSERT INTO t VALUES(1,1),(2,0),(3,5),(4,2),(5,0),(6,3)`,
			`UPDATE t SET b=(SELECT sum(b) FROM t t2 WHERE t2.a<=t.a) WHERE b>0`, `SELECT a,b FROM t ORDER BY a`},
		"indexed-by-full": {`CREATE TABLE t(a,b)`, `CREATE INDEX tb ON t(b)`, `INSERT INTO t VALUES(1,1),(2,0),(3,5),(4,2),(5,0),(6,3)`,
			`UPDATE t INDEXED BY tb SET b=(SELECT sum(b) FROM t t2 WHERE t2.a<=t.a)`, `SELECT a,b FROM t ORDER BY a`},
		"without-rowid": {`CREATE TABLE t(a PRIMARY KEY,b) WITHOUT ROWID`, `INSERT INTO t VALUES(3,3),(1,1),(2,2),(5,5),(4,4)`,
			`UPDATE t SET b=(SELECT sum(b) FROM t t2 WHERE t2.a<=t.a)`, `SELECT a,b FROM t ORDER BY a`},
		"text-index-first-row": {`CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)`, `CREATE INDEX tv ON t(v)`, `INSERT INTO t VALUES(1,'d'),(2,'b'),(3,'a'),(4,'c')`,
			`UPDATE t SET v=(SELECT v FROM t t2 WHERE t2.id<=t.id) WHERE v>'a'`, `SELECT id,v FROM t ORDER BY id`},
		"analyzed-updated-index": {`CREATE TABLE t(a,b)`, `CREATE INDEX tb ON t(b)`, `INSERT INTO t VALUES(1,1),(2,0),(3,5),(4,2),(5,0),(6,3)`, `ANALYZE`,
			`UPDATE t SET b=(SELECT sum(b) FROM t t2 WHERE t2.a<>t.a) WHERE b>0`, `SELECT a,b FROM t ORDER BY a`},
		"upsert-select-source": {`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE s(a,b)`, `INSERT INTO t VALUES(1,1,0),(2,2,0)`, `INSERT INTO s VALUES(1,10),(3,30),(2,20)`,
			`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT(a) DO UPDATE SET c=(SELECT count(*) FROM s)`, `SELECT * FROM t ORDER BY a`},
	}
	// Each arm of updateOnePassOff: the planner drives the scan through tk, and
	// something else about the statement makes C run it ONEPASS_OFF -- rowid
	// order -- anyway (update.c:733-739, :757-766; where.c:7227).
	off := `INSERT INTO t(id,k,u,n) VALUES(1,30,1,1),(2,10,2,2),(3,20,3,3),(4,10,4,4),(5,40,5,5)`
	sum := `(SELECT sum(n) FROM t AS x WHERE x.id <> t.id)`
	offCases := map[string][]string{
		"or-replace":    {`CREATE TABLE t(id INTEGER PRIMARY KEY, k, u, n)`, `CREATE INDEX tk ON t(k)`, off, `UPDATE OR REPLACE t SET n = ` + sum + ` WHERE k > 15`},
		"index-replace": {`CREATE TABLE t(id INTEGER PRIMARY KEY, k, u UNIQUE ON CONFLICT REPLACE, n)`, `CREATE INDEX tk ON t(k)`, off, `UPDATE t SET u = u, n = ` + sum + ` WHERE k > 15`},
		"fk-child":      {`PRAGMA foreign_keys=ON`, `CREATE TABLE p(pk PRIMARY KEY)`, `INSERT INTO p VALUES(1),(2),(3),(4),(5)`, `CREATE TABLE t(id INTEGER PRIMARY KEY, k, u REFERENCES p(pk), n)`, `CREATE INDEX tk ON t(k)`, off, `UPDATE t SET u = u, n = ` + sum + ` WHERE k > 15`},
		"fk-parent":     {`PRAGMA foreign_keys=ON`, `CREATE TABLE t(id INTEGER PRIMARY KEY, k, u UNIQUE, n)`, `CREATE INDEX tk ON t(k)`, `CREATE TABLE ch(c REFERENCES t(u))`, off, `UPDATE t SET u = u, n = ` + sum + ` WHERE k > 15`},
		"rowid-change":  {`CREATE TABLE t(id INTEGER PRIMARY KEY, k, u, n)`, `CREATE INDEX tk ON t(k)`, off, `UPDATE t SET id = id, n = ` + sum + ` WHERE k > 15`},
		"multi-or":      {`CREATE TABLE t(id INTEGER PRIMARY KEY, k, u, n)`, `CREATE INDEX tk ON t(k)`, `CREATE INDEX tu ON t(u)`, off, `UPDATE t SET n = ` + sum + ` WHERE k = 40 OR u = 2 OR k = 20`},
		"index-kept":    {`CREATE TABLE t(id INTEGER PRIMARY KEY, k, u, n)`, `CREATE INDEX tk ON t(k)`, off, `UPDATE t SET u = u, n = ` + sum + ` WHERE k > 15`},
	}
	for name, stmts := range offCases {
		differ(t, "onepass-off/"+name, append(stmts, `SELECT * FROM t ORDER BY id`))
	}
	for name, stmts := range promoted {
		differ(t, "promoted/"+name, stmts)
	}
	for name, stmts := range bodyCases {
		differ(t, "body/"+name, append(append([]string{}, body...), stmts...))
	}
	for name, stmts := range upsCases {
		differ(t, "upsert/"+name, append(append([]string{}, ups...), stmts...))
	}
}
