// The table editor against the package, under Node: node schema.test.mjs
import assert from "node:assert/strict";
import { open } from "../../npm/index.js";
import { applyEdit, blankColumn, planEdit, readTable } from "./src/schema.js";

const raw = await open();
const db = {
	run: async (sql, p = []) => raw.run(sql, ...p),
	rows: async (sql, p = []) => raw.all(sql, ...p),
};
raw.exec(`CREATE TABLE artist(id INTEGER PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE track(id INTEGER PRIMARY KEY, title TEXT, artist INTEGER REFERENCES artist(id) ON DELETE CASCADE, plays INTEGER DEFAULT 0, note TEXT);
CREATE INDEX track_title ON track(title);
CREATE VIEW popular AS SELECT title FROM track WHERE plays > 10;
CREATE TRIGGER track_ins AFTER INSERT ON track BEGIN UPDATE track SET plays = plays + 1 WHERE id = new.id; END;
INSERT INTO artist VALUES (1, 'a'), (2, 'b');
INSERT INTO track(title, artist, plays, note) VALUES ('x', 1, 20, 'n1'), ('y', 2, 5, NULL), ('z', NULL, '7', 'n3');
PRAGMA foreign_keys = ON;`);

const edit = async (f) => {
	const old = await readTable(db, "track");
	const next = structuredClone(old);
	f(next);
	const plan = planEdit(old, next);
	await applyEdit(db, old, next);
	return plan;
};
const before = raw.all("SELECT * FROM track ORDER BY id");

let m = await readTable(db, "track");
assert.deepEqual(m.unmodeled, []);
assert.equal(m.cols.find((c) => c.name === "artist").ref, "artist(id)");
assert.equal(m.cols.find((c) => c.name === "artist").onDelete, "CASCADE");

// A rename and an appended column are plain ALTERs.
let p = await edit((n) => {
	n.cols.find((c) => c.name === "note").name = "memo";
	n.cols.push({ ...blankColumn(), name: "added", type: "INTEGER", dflt: "3" });
});
assert.equal(p.rebuild, false);
assert.deepEqual(raw.all("SELECT memo, added FROM track ORDER BY id"), [{ memo: "n1", added: 3 }, { memo: null, added: 3 }, { memo: "n3", added: 3 }]);

// A type change and a reorder rebuild, keeping rows, index, view, trigger and the foreign key.
p = await edit((n) => {
	n.cols.find((c) => c.name === "plays").type = "TEXT";
	n.cols.reverse();
});
assert.equal(p.rebuild, true);
assert.deepEqual(raw.all("SELECT id, title, artist, plays FROM track ORDER BY id"), before.map((r) => ({ id: r.id, title: r.title, artist: r.artist, plays: String(r.plays) })));
const names = raw.all("SELECT type, name FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' ORDER BY name").map((o) => `${o.type} ${o.name}`);
assert.deepEqual(names, ["table artist", "view popular", "table track", "trigger track_ins", "index track_title"]);
assert.equal((await readTable(db, "track")).cols.find((c) => c.name === "artist").ref, "artist(id)");
raw.run("DELETE FROM artist WHERE id = 1");
assert.equal(raw.get("SELECT count(*) AS n FROM track").n, 2, "ON DELETE CASCADE survives the rebuild");
assert.equal(raw.get("PRAGMA foreign_keys").foreign_keys, 1);

// NOT NULL on a column holding a NULL fails, and nothing changes.
const sql = raw.get("SELECT sql FROM sqlite_schema WHERE name = 'track'").sql;
await assert.rejects(edit((n) => { n.cols.find((c) => c.name === "artist").notnull = true; }));
assert.equal(raw.get("SELECT sql FROM sqlite_schema WHERE name = 'track'").sql, sql);
assert.equal(raw.get("SELECT count(*) AS n FROM track").n, 2);

// Dropping a column the index uses fails the same way.
await assert.rejects(edit((n) => { n.cols = n.cols.filter((c) => c.name !== "title"); n.cols[0].type = "ANY"; }), /track_title/);
assert.equal(raw.get("SELECT sql FROM sqlite_schema WHERE name = 'track'").sql, sql);

// A foreign key the rebuild would break is refused.
await assert.rejects(edit((n) => { n.cols.find((c) => c.name === "memo").ref = "artist(id)"; }), /foreign key/);

// A rename of the table with a change.
await edit((n) => { n.name = "song"; n.cols.find((c) => c.name === "memo").type = "BLOB"; });
// plays is TEXT now, and text sorts above every number.
assert.equal(raw.get("SELECT count(*) AS n FROM popular").n, 2);

// What the form cannot hold is reported, not dropped.
raw.exec("CREATE TABLE c(a INTEGER CHECK (a > 0), b TEXT COLLATE NOCASE)");
assert.deepEqual((await readTable(db, "c")).unmodeled, ["a CHECK constraint", "a COLLATE clause"]);
console.log("schema ok");
