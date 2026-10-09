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
