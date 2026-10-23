// dumpSQL is the open database as a SQL script, the way the sqlite3 shell's
// .dump writes one: each table's CREATE and its rows as INSERTs, then the
// indexes, views and triggers. It is how a remote database is downloaded,
// since the file export reads the local engine's bytes.
//
// A virtual table's CREATE is written but not its rows: its module rebuilds
// its own shadow tables, which are left out (named <vtab>_<suffix>).
import { ident, query, rows } from "./db.js";
import { literal } from "./values.js";

export async function dumpSQL() {
	const objs = await rows("SELECT type, name, tbl_name, sql FROM sqlite_schema WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY rowid");
	const vtabs = objs.filter((o) => /^\s*CREATE\s+VIRTUAL\b/i.test(o.sql)).map((o) => o.name);
	const shadow = (name) => vtabs.some((v) => name.startsWith(`${v}_`));
	const out = ["PRAGMA foreign_keys=OFF;", "BEGIN TRANSACTION;"];
	for (const o of objs) {
		if (o.type !== "table" || shadow(o.name)) continue;
		out.push(`${o.sql};`);
		if (vtabs.includes(o.name)) continue;
		const r = await query(`SELECT * FROM ${ident(o.name)}`);
		for (const row of r.rows) out.push(`INSERT INTO ${ident(o.name)} VALUES(${row.map(literal).join(",")});`);
	}
	for (const type of ["index", "view", "trigger"]) for (const o of objs) if (o.type === type && !shadow(o.tbl_name)) out.push(`${o.sql};`);
	out.push("COMMIT;");
	return out.join("\n") + "\n";
}
