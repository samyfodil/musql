// Table editing: a table's definition as a form's model, and applying an edited
// model. db is {run(sql, params), rows(sql, params)}, async, so this runs in the
// page's worker and in Node.

/** A quoted SQL identifier (as db.js's; this module also runs in Node). */
const ident = (s) => `"${String(s).replace(/"/g, '""')}"`;

// Definitions the model cannot hold; such a table is edited as SQL instead.
const UNMODELED = [
	[/\bCHECK\s*\(/i, "a CHECK constraint"],
	[/\bCOLLATE\b/i, "a COLLATE clause"],
	[/\bGENERATED\b|\bAS\s*\(/i, "a generated column"],
	[/\bAUTOINCREMENT\b/i, "AUTOINCREMENT"],
	[/\bCONSTRAINT\b/i, "a named constraint"],
	[/\bON\s+CONFLICT\b/i, "an ON CONFLICT clause"],
	[/\bDEFERRABLE\b|\bMATCH\b|\bON\s+UPDATE\b/i, "a foreign key option"],
	[/\bASC\b|\bDESC\b/i, "a key order"],
];

/** sql with its string literals, quoted names and comments blanked, for keyword tests. */
function bare(sql) {
	return sql.replace(/'(?:[^']|'')*'|"(?:[^"]|"")*"|`[^`]*`|\[[^\]]*\]|--[^\n]*|\/\*[\s\S]*?\*\//g, " ");
}

/**
 * The model of table name: {name, cols, withoutRowid, strict, unmodeled}, each
 * column {orig, name, type, pk, notnull, unique, dflt, ref, onDelete} with orig
 * its current name (null for a new column). unmodeled lists what the form
 * cannot represent.
 */
export async function readTable(db, name) {
	const [obj] = await db.rows("SELECT sql FROM sqlite_schema WHERE type = 'table' AND name = ?", [name]);
	const sql = obj?.sql ?? "";
	const info = await db.rows(`PRAGMA table_xinfo(${ident(name)})`);
	const fks = await db.rows(`PRAGMA foreign_key_list(${ident(name)})`);
	const idx = await db.rows(`PRAGMA index_list(${ident(name)})`);
	const b = bare(sql);
	const unmodeled = [];
	if (/^\s*CREATE\s+VIRTUAL\b/i.test(sql)) unmodeled.push("a virtual table");
	for (const [re, what] of UNMODELED) if (re.test(b)) unmodeled.push(what);
	if (info.some((c) => c.hidden)) unmodeled.push("a hidden or generated column");

	const unique = new Set();
	for (const i of idx.filter((i) => i.origin === "u")) {
		const cs = await db.rows(`PRAGMA index_info(${ident(i.name)})`);
		if (cs.length === 1) unique.add(cs[0].name);
		else unmodeled.push("a UNIQUE over several columns");
	}
	const ref = {};
	const byId = Map.groupBy ? Map.groupBy(fks, (f) => f.id) : groupBy(fks);
	for (const group of byId.values()) {
		if (group.length > 1) {
			unmodeled.push("a foreign key over several columns");
			continue;
		}
		const f = group[0];
		ref[f.from] = { ref: f.to == null ? f.table : `${f.table}(${f.to})`, onDelete: f.on_delete === "NO ACTION" ? "" : f.on_delete };
	}

	return {
		name,
		withoutRowid: /\bWITHOUT\s+ROWID\b/i.test(b),
		strict: /\)\s*(?:WITHOUT\s+ROWID\s*,\s*)?STRICT\b/i.test(b),
		unmodeled: [...new Set(unmodeled)],
		cols: info.map((c) => ({
			orig: c.name,
			name: c.name,
			type: c.type,
			pk: c.pk > 0,
			pkOrder: c.pk,
			notnull: !!c.notnull,
			unique: unique.has(c.name),
			dflt: c.dflt_value ?? "",
			ref: ref[c.name]?.ref ?? "",
			onDelete: ref[c.name]?.onDelete ?? "",
		})),
	};
}

function groupBy(list) {
	const m = new Map();
	for (const f of list) m.set(f.id, [...(m.get(f.id) ?? []), f]);
	return m;
}

/** A new table's model: an INTEGER PRIMARY KEY and one text column. */
export function blankTable() {
	return {
		name: "",
		withoutRowid: false,
		strict: false,
		unmodeled: [],
		cols: [
			{ orig: null, name: "id", type: "INTEGER", pk: true, notnull: false, unique: false, dflt: "", ref: "", onDelete: "" },
			blankColumn(),
		],
	};
}

export const blankColumn = () => ({ orig: null, name: "", type: "TEXT", pk: false, notnull: false, unique: false, dflt: "", ref: "", onDelete: "" });

/** ref as a REFERENCES target: "t" or "t(c)", the names quoted. */
function target(ref) {
	const m = /^\s*([^()]+?)\s*(?:\(\s*([^()]*?)\s*\))?\s*$/.exec(ref);
	if (!m) return ref;
	return m[2] ? `${ident(m[1])}(${ident(m[2])})` : ident(m[1]);
}

/** Column c's definition; single says whether its PRIMARY KEY is a column constraint. */
function colDef(c, single) {
	return [
		ident(c.name),
		c.type.trim(),
		single && c.pk ? "PRIMARY KEY" : "",
		c.notnull ? "NOT NULL" : "",
		c.unique && !(single && c.pk) ? "UNIQUE" : "",
		c.dflt !== "" ? `DEFAULT ${defaultSQL(c.dflt)}` : "",
		c.ref.trim() ? `REFERENCES ${target(c.ref)}${c.onDelete ? ` ON DELETE ${c.onDelete}` : ""}` : "",
	].filter(Boolean).join(" ");
}

/** The CREATE TABLE statement of model m, named as. */
export function createSQL(m, as = m.name) {
	const cols = m.cols.filter((c) => c.name.trim());
	const pks = cols.filter((c) => c.pk).sort((a, b) => (a.pkOrder || 99) - (b.pkOrder || 99));
	const defs = cols.map((c) => colDef(c, pks.length === 1));
	if (pks.length > 1) defs.push(`PRIMARY KEY(${pks.map((c) => ident(c.name)).join(", ")})`);
	const opts = [m.withoutRowid && "WITHOUT ROWID", m.strict && "STRICT"].filter(Boolean).join(", ");
	return `CREATE TABLE ${ident(as || "…")}(\n  ${defs.join(",\n  ")}\n)${opts ? " " + opts : ""}`;
}

// A default is shown as SQL; one the grammar does not take bare is
// parenthesized.
function defaultSQL(d) {
	const s = String(d).trim();
	if (/^(?:[-+]?\d+(?:\.\d+)?(?:e[-+]?\d+)?|'(?:[^']|'')*'|NULL|TRUE|FALSE|CURRENT_(?:TIME|DATE|TIMESTAMP)|x'[0-9a-f]*'|\(.*\))$/is.test(s)) return s;
	return `(${s})`;
}

const same = (a, b, keys) => keys.every((k) => String(a[k]) === String(b[k]));

/**
 * The plan for turning model old into next: {renames, steps, rebuild}. The
 * RENAME COLUMNs run first, then either plain ALTERs or a rebuild.
 */
export function planEdit(old, next) {
	const cols = next.cols.filter((c) => c.name.trim());
	const kept = cols.filter((c) => c.orig != null);
	const added = cols.filter((c) => c.orig == null);
	const dropped = old.cols.filter((o) => !kept.some((c) => c.orig === o.orig));
	const renames = kept
		.filter((c) => c.name !== c.orig)
		.map((c) => `ALTER TABLE ${ident(old.name)} RENAME COLUMN ${ident(c.orig)} TO ${ident(c.name)}`);

	const unchanged = kept.every((c) => {
		const o = old.cols.find((x) => x.orig === c.orig);
		return same(o, c, ["type", "pk", "notnull", "unique", "dflt", "ref", "onDelete"]);
	});
	// ADD COLUMN appends, so the kept columns must lead, in their old order.
	const rest = old.cols.filter((o) => !dropped.includes(o));
	const order = cols.slice(0, kept.length).every((c, i) => c.orig === rest[i].orig);
	// ADD COLUMN takes no key, no UNIQUE, and a constant default when NOT NULL;
	// DROP COLUMN one nothing uses. The engine refuses the rest, and the rebuild
	// is offered.
	const simpleAdd = (c) => !c.pk && !c.unique && (!c.notnull || c.dflt !== "") && !/^\(|CURRENT_/i.test(String(c.dflt).trim());
	const simpleDrop = (c) => !c.pk && !c.unique;
	const tableOpts = old.withoutRowid === next.withoutRowid && old.strict === next.strict;
	const rebuild = !(unchanged && order && tableOpts && added.every(simpleAdd) && dropped.every(simpleDrop));
	const steps = rebuild
		? []
		: [
			...dropped.map((c) => `ALTER TABLE ${ident(old.name)} DROP COLUMN ${ident(c.orig)}`),
			...added.map((c) => `ALTER TABLE ${ident(old.name)} ADD COLUMN ${colDef(c, true)}`),
		];
	return { renames, steps, rebuild, kept, dropped };
}

/** The statements an edit runs, as a script to read (the rebuild's index and trigger re-creation abridged). */
export function previewEdit(old, next) {
	const p = planEdit(old, next);
	const out = [...p.renames];
	const rename = next.name && next.name !== old.name ? [`ALTER TABLE ${ident(old.name)} RENAME TO ${ident(next.name)}`] : [];
	if (!p.rebuild) return [...out, ...p.steps, ...rename];
	const tmp = `${old.name}__new`;
	const names = p.kept.map((c) => ident(c.name)).join(", ");
	out.push(
		createSQL(next, tmp),
		`INSERT INTO ${ident(tmp)}(${names}) SELECT ${names} FROM ${ident(old.name)}`,
		`DROP TABLE ${ident(old.name)}`,
		`ALTER TABLE ${ident(tmp)} RENAME TO ${ident(old.name)}`,
		"-- then its indexes and triggers, and the views and triggers that name it, are created again",
		...rename,
	);
	return out;
}

/**
 * applyEdit changes table old to model next in one transaction, rolled back on
 * any error. A rebuild is SQLite's documented procedure (new table, copy, drop,
 * rename) with foreign keys off for its length and checked before the commit.
 */
export async function applyEdit(db, old, next) {
	const p = planEdit(old, next);
	const [{ foreign_keys: fkOn }] = await db.rows("PRAGMA foreign_keys");
	if (fkOn) await db.run("PRAGMA foreign_keys = OFF");
	await db.run("BEGIN");
	try {
		for (const s of p.renames) await db.run(s);
		if (p.rebuild) await rebuild(db, old.name, next, p.kept);
		else for (const s of p.steps) await db.run(s);
		if (next.name && next.name !== old.name) await db.run(`ALTER TABLE ${ident(old.name)} RENAME TO ${ident(next.name)}`);
		if (fkOn) {
			const bad = await db.rows("PRAGMA foreign_key_check");
			if (bad.length) throw new Error(`the change breaks ${bad.length} foreign key${bad.length > 1 ? "s" : ""} (${bad[0].table} → ${bad[0].parent})`);
		}
		await db.run("COMMIT");
	} catch (e) {
		await db.run("ROLLBACK").catch(() => {});
		throw e;
	} finally {
		if (fkOn) await db.run("PRAGMA foreign_keys = ON");
	}
}

async function rebuild(db, name, next, kept) {
	const lower = name.toLowerCase();
	const all = await db.rows("SELECT type, name, tbl_name, sql FROM sqlite_schema WHERE sql IS NOT NULL ORDER BY rowid");
	// Views and other tables' triggers that name the table go too and come back
	// after it, since the rename into place checks every one.
	const mentions = (o) => o.sql.toLowerCase().includes(lower);
	const own = all.filter((o) => o.tbl_name === name && (o.type === "index" || o.type === "trigger"));
	const views = all.filter((o) => o.type === "view" && mentions(o));
	const others = all.filter((o) => o.type === "trigger" && o.tbl_name !== name && mentions(o));
	for (const v of [...others].reverse()) await db.run(`DROP TRIGGER ${ident(v.name)}`);
	for (const v of [...views].reverse()) await db.run(`DROP VIEW ${ident(v.name)}`);

	const tmp = `${name}__new`;
	const names = kept.map((c) => ident(c.name)).join(", ");
	await db.run(createSQL(next, tmp));
	await db.run(`INSERT INTO ${ident(tmp)}(${names}) SELECT ${names} FROM ${ident(name)}`);
	await db.run(`DROP TABLE ${ident(name)}`);
	await db.run(`ALTER TABLE ${ident(tmp)} RENAME TO ${ident(name)}`);

	for (const o of [...own.filter((o) => o.type === "index"), ...views, ...own.filter((o) => o.type === "trigger"), ...others]) {
		try {
			await db.run(o.sql);
		} catch (e) {
			throw new Error(`${o.type} ${o.name} no longer fits the table (${e.message}); drop or change it first`);
		}
	}
}
