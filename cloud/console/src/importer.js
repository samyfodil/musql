// CSV and JSON into a table: parse, infer each column's type, create the table
// (or append to one), insert in one transaction.
import { ident, run } from "./db.js";

/** Parses CSV text (RFC 4180: quoted fields, "" inside them, CRLF or LF) into rows of strings. */
export function parseCSV(text) {
	const rows = [];
	let row = [];
	let field = "";
	let quoted = false;
	let i = 0;
	if (text.charCodeAt(0) === 0xfeff) i = 1; // a byte-order mark
	for (; i < text.length; i++) {
		const c = text[i];
		if (quoted) {
			if (c === '"') {
				if (text[i + 1] === '"') {
					field += '"';
					i++;
				} else quoted = false;
			} else field += c;
		} else if (c === '"' && field === "") quoted = true;
		else if (c === ",") {
			row.push(field);
			field = "";
		} else if (c === "\n" || c === "\r") {
			if (c === "\r" && text[i + 1] === "\n") i++;
			row.push(field);
			rows.push(row);
			row = [];
			field = "";
		} else field += c;
	}
	if (field !== "" || row.length) {
		row.push(field);
		rows.push(row);
	}
	return rows.filter((r) => r.length > 1 || r[0] !== "");
}

/** {columns, rows} from a JSON array of objects (or of arrays, with no column names). */
export function parseJSON(text) {
	const data = JSON.parse(text);
	const list = Array.isArray(data) ? data : Array.isArray(data?.rows) ? data.rows : null;
	if (!list) throw new Error("the JSON is not an array of rows");
	if (list.every(Array.isArray)) {
		const n = Math.max(0, ...list.map((r) => r.length));
		return { columns: Array.from({ length: n }, (_, i) => `c${i + 1}`), rows: list };
	}
	const columns = [];
	for (const o of list) for (const k of Object.keys(o ?? {})) if (!columns.includes(k)) columns.push(k);
	return { columns, rows: list.map((o) => columns.map((c) => o?.[c])) };
}

const INT = /^[+-]?\d{1,15}$/;
const REAL = /^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$/;

/** Each column's type: INTEGER, REAL or TEXT, by what all its non-empty values are. */
export function inferTypes(columns, rows) {
	return columns.map((_, j) => {
		let type = null;
		for (const r of rows) {
			const v = r[j];
			if (v === null || v === undefined || v === "") continue;
			const s = typeof v === "string" ? v.trim() : v;
			const t = typeof s === "number" ? (Number.isInteger(s) ? "INTEGER" : "REAL") : typeof s === "boolean" ? "INTEGER" : INT.test(s) ? "INTEGER" : REAL.test(s) ? "REAL" : "TEXT";
			if (t === "TEXT") return "TEXT";
			if (type === null || (type === "INTEGER" && t === "REAL")) type = t;
		}
		return type ?? "TEXT";
	});
}

/** A cell as its column's type stores it: "" is NULL, a number column's text a number. */
function cell(v, type) {
	if (v === null || v === undefined || v === "") return null;
	if (typeof v === "boolean") return v ? 1 : 0;
	if (typeof v === "object") return JSON.stringify(v);
	if (typeof v === "string" && type !== "TEXT") {
		const s = v.trim();
		if (REAL.test(s)) return Number(s);
	}
	return v;
}

/**
 * Inserts rows into table, creating it with types when create is set. 200
 * rows to a statement, all in one transaction. progress(done) after each.
 */
export async function importRows(table, columns, types, rows, create, progress = () => {}) {
	const t = ident(table);
	await run("BEGIN");
	try {
		if (create) await run(`CREATE TABLE ${t}(${columns.map((c, j) => `${ident(c)} ${types[j]}`).join(", ")})`);
		const cols = columns.map(ident).join(", ");
		const one = `(${columns.map(() => "?").join(", ")})`;
		const per = Math.max(1, Math.min(200, Math.floor(30000 / Math.max(1, columns.length))));
		for (let i = 0; i < rows.length; i += per) {
			const chunk = rows.slice(i, i + per);
			await run(`INSERT INTO ${t}(${cols}) VALUES ${chunk.map(() => one).join(", ")}`, chunk.flatMap((r) => columns.map((_, j) => cell(r[j], types[j]))));
			progress(Math.min(i + per, rows.length));
		}
		await run("COMMIT");
	} catch (e) {
		await run("ROLLBACK").catch(() => {});
		throw e;
	}
}
