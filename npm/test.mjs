// The package's own test, under Node: npm run build && npm test.
import assert from "node:assert/strict";
import { open } from "./index.js";

const db = await open();
db.exec("CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT, n INTEGER, f REAL, b BLOB)");

const r = db.run("INSERT INTO t(name, n, f, b) VALUES (?, ?, ?, ?)", "ada", 42, 1.5, Uint8Array.of(0, 1, 255));
assert.deepEqual(r, { changes: 1, lastInsertRowid: 1 });
db.run("INSERT INTO t(name, n) VALUES (?, ?)", "grace\u0000é", 2n ** 62n);
db.run("INSERT INTO t(name, n) VALUES (?, ?)", null, true);

assert.deepEqual(db.get("SELECT name, n, f, b FROM t WHERE id = ?", 1), {
	name: "ada", n: 42, f: 1.5, b: Uint8Array.of(0, 1, 255),
});
assert.deepEqual(db.get("SELECT name, n FROM t WHERE id = 2"), { name: "grace\u0000é", n: 2n ** 62n });
assert.deepEqual(db.get("SELECT name, n FROM t WHERE id = 3"), { name: null, n: 1 });
assert.deepEqual(db.query("SELECT count(*), sum(n) FROM t WHERE n < 100"), { columns: ["count(*)", "sum(n)"], rows: [[2, 43]] });
assert.deepEqual(db.all("SELECT id FROM t ORDER BY id"), [{ id: 1 }, { id: 2 }, { id: 3 }]);
assert.equal(db.get("SELECT 1 WHERE 0"), undefined);

// A wrong statement throws its error and leaves the database usable.
assert.throws(() => db.all("SELECT nosuch FROM t"), /no such column: nosuch/);
assert.equal(db.get("SELECT count(*) AS c FROM t").c, 3);

// A bigger table, through the columnar paths and after writes to it.
db.exec("CREATE TABLE big(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER)");
db.run("WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 50000) INSERT INTO big SELECT i, i % 10, i * 3 FROM c");
db.exec("VACUUM");
assert.deepEqual(db.query("SELECT count(*), sum(v) FROM big").rows, [[50000, 3750075000]]);
db.run("UPDATE big SET v = v + 1 WHERE id = ?", 7);
db.run("DELETE FROM big WHERE id = (SELECT max(id) FROM big)");
assert.deepEqual(db.query("SELECT count(*), sum(v) FROM big").rows, [[49999, 3750075000 + 1 - 150000]]);
assert.equal(db.query("SELECT k, count(*) FROM big GROUP BY k").rows.length, 10);

// export() and open(..., { data }) move a database out and back in.
const bytes = db.export();
assert.ok(bytes instanceof Uint8Array && bytes.length > 0);
const copy = await open("copy.db", { data: bytes });
assert.deepEqual(copy.query("SELECT count(*), sum(v) FROM big").rows, db.query("SELECT count(*), sum(v) FROM big").rows);
copy.run("DELETE FROM t");
assert.equal(db.get("SELECT count(*) AS c FROM t").c, 3, "two databases are independent");

db.close();
assert.throws(() => db.all("SELECT 1"), /closed/);
console.log("ok");

// SQLite files: out to C SQLite's format and back in, with a header check.
{
	const src = await open();
	src.exec("CREATE TABLE p(id INTEGER PRIMARY KEY, name TEXT UNIQUE, b BLOB); CREATE INDEX pb ON p(b)");
	src.run("INSERT INTO p(name, b) VALUES (?, ?), (?, ?)", "x", Uint8Array.of(7), "y", null);
	const file = src.exportSQLite();
	assert.equal(new TextDecoder().decode(file.subarray(0, 15)), "SQLite format 3");
	const back = await open("from-sqlite.db", { data: file });
	assert.deepEqual(back.all("SELECT * FROM p ORDER BY id"), [{ id: 1, name: "x", b: Uint8Array.of(7) }, { id: 2, name: "y", b: null }]);
	assert.equal(back.get("SELECT count(*) AS n FROM sqlite_schema WHERE type = 'index'").n, 2);
	await assert.rejects(open("bad.db", { data: file.slice(0, 200) }));
	console.log("sqlite import/export ok");
}
