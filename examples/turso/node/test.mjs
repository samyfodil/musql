// Turso's Node client (@libsql/client) against musqld: the URL forms Turso apps
// use (http://, ws://, libsql://) and the client's whole surface.
//
//   musqld -db t.musq -listen 127.0.0.1:8080 -auth-token tok
//   node test.mjs http://127.0.0.1:8080
import { createClient } from "@libsql/client";
import assert from "node:assert/strict";

const url = process.argv[2];
const db = createClient({ url, authToken: "tok", intMode: "bigint" });
const log = (...a) => console.log(...a);

await db.execute("CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, score REAL, big INTEGER, data BLOB)");
let r = await db.execute({ sql: "INSERT INTO t(name, score, big, data) VALUES(?, ?, ?, ?)", args: ["ada", 1.5, 2n ** 62n, new Uint8Array([0, 1, 2])] });
assert.equal(r.rowsAffected, 1);
assert.equal(r.lastInsertRowid, 1n);
await db.execute({ sql: "INSERT INTO t(name, score) VALUES(:n, :s)", args: { n: "bob", s: 2.25 } });

r = await db.execute({ sql: "SELECT id, name, score, big, data FROM t WHERE id = ?", args: [1] });
assert.deepEqual(r.columns, ["id", "name", "score", "big", "data"]);
assert.equal(r.columnTypes[1], "TEXT");
const row = r.rows[0];
assert.equal(row.name, "ada");
assert.equal(row.score, 1.5);
assert.equal(row.big, 2n ** 62n);
assert.deepEqual(new Uint8Array(row.data), new Uint8Array([0, 1, 2]));
assert.equal((await db.execute("SELECT big FROM t WHERE name = 'bob'")).rows[0].big, null);

await assert.rejects(db.execute("INSERT INTO t(name) VALUES('ada')"), (e) => {
  log("constraint error:", e.code, "-", e.message);
  return /SQLITE_CONSTRAINT/.test(e.code);
});

// batch: one transaction, all or nothing
const br = await db.batch(["INSERT INTO t(name) VALUES('carol')", { sql: "INSERT INTO t(name) VALUES(?)", args: ["dan"] }], "write");
assert.equal(br.length, 2);
await assert.rejects(db.batch(["INSERT INTO t(name) VALUES('eve')", "INSERT INTO t(name) VALUES('ada')"], "write"));
assert.equal((await db.execute("SELECT count(*) AS n FROM t WHERE name = 'eve'")).rows[0].n, 0n, "a failed batch must roll back");

// interactive transaction
let tx = await db.transaction("write");
await tx.execute("INSERT INTO t(name) VALUES('ghost')");
assert.equal((await tx.execute("SELECT count(*) AS n FROM t")).rows[0].n, 5n);
await tx.rollback();
tx = await db.transaction("write");
await tx.execute("INSERT INTO t(name) VALUES('frank')");
await tx.commit();
assert.equal((await db.execute("SELECT count(*) AS n FROM t")).rows[0].n, 5n);

// executeMultiple: a script
await db.executeMultiple("CREATE TABLE u(x); INSERT INTO u VALUES(1); INSERT INTO u VALUES(2);");
assert.equal((await db.execute("SELECT sum(x) AS s FROM u")).rows[0].s, 3n);

// read transaction and migrate
tx = await db.transaction("read");
assert.equal((await tx.execute("SELECT count(*) AS n FROM u")).rows[0].n, 2n);
await tx.commit();
await db.migrate(["CREATE TABLE IF NOT EXISTS v(y)", "INSERT INTO v VALUES(7)"]);
assert.equal((await db.execute("SELECT y FROM v")).rows[0].y, 7n);

db.close();
log("ALL OK", url);
