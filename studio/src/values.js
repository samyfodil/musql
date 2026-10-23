// How a value looks in a grid: its text, a class for NULL, numbers, blobs and
// vectors, and the fuller text a hover shows.

const VECTOR_TYPE = /^(F32_BLOB|FLOAT32)\s*\(\s*\d+\s*\)$/i;

/**
 * Whether a column holds float32 vectors: by its declared type, or, for a
 * plain BLOB column or a result column with no type (an expression), by a
 * name that says so (vec1 declares its vector column a BLOB named "vector").
 */
export const isVectorColumn = (name, type) => VECTOR_TYPE.test(type ?? "") || ((!type || /^BLOB$/i.test(type)) && /(_emb|embedding|vector)$/i.test(name ?? ""));

/** The components of a float32 vector blob. */
export const vectorOf = (b) => new Float32Array(b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength - (b.byteLength % 4)));

const num = (x) => (Math.abs(x) >= 1e4 || (x !== 0 && Math.abs(x) < 1e-3) ? x.toExponential(2) : x.toFixed(3));

const NUMERIC = /INT|REAL|FLOA|DOUB|NUM|DEC/i;
// Numbers that are names, not quantities: ids, years, codes. They never get separators.
const isKey = (name) => !name || /^(id|rowid|year|yr|zip|zipcode|postcode|port|pin|code)$|_id$|_year$|_code$/i.test(name);

/**
 * v as a grid shows it: {text, cls, title, long}. vector says its column holds
 * vectors; type and name (the column's) pick how a number or a flag reads:
 * a large number gets separators unless it names a row (id, *_id), and a
 * BOOLEAN column shows true and false. long marks a value worth opening whole
 * (long text, JSON).
 */
export function show(v, vector = false, type = "", name = "") {
	if (v === null || v === undefined) return { text: "NULL", cls: "null" };
	if (v instanceof Uint8Array) {
		if (vector && v.length % 4 === 0 && v.length > 0) {
			const x = vectorOf(v);
			const head = Array.from(x.subarray(0, 4), num).join(", ");
			return {
				text: `[${head}${x.length > 4 ? ", …" : ""}] · ${x.length}d`,
				cls: "vec",
				title: `${x.length} dimensions\n[${Array.from(x.subarray(0, 64), num).join(", ")}${x.length > 64 ? ", …" : ""}]`,
			};
		}
		const hex = Array.from(v.subarray(0, 8), (b) => b.toString(16).padStart(2, "0")).join("");
		return { text: `BLOB · ${bytes(v.length)}`, cls: "blob", title: `x'${hex}${v.length > 8 ? "…" : ""}'` };
	}
	if (typeof v === "number" || typeof v === "bigint") {
		if (/BOOL/i.test(type) && (v == 0 || v == 1)) return { text: v == 1 ? "true" : "false", cls: v == 1 ? "bool on" : "bool" };
		const sep = name && !isKey(name) && NUMERIC.test(type) && Math.abs(Number(v)) >= 1000;
		return { text: sep ? v.toLocaleString() : String(v), cls: "num", title: sep ? String(v) : undefined };
	}
	const s = String(v);
	if (/^\s*[[{]/.test(s) && s.length > 1) {
		try {
			JSON.parse(s);
			return { text: s.length > 200 ? s.slice(0, 200) + "…" : s, cls: "json", long: true };
		} catch {
			// text that only starts like JSON
		}
	}
	const long = s.length > 60 || s.includes("\n");
	return { text: s.length > 300 ? s.slice(0, 300).replace(/\n/g, " ↵ ") + "…" : s.replace(/\n/g, " ↵ "), cls: "", title: long ? s.slice(0, 1000) : undefined, long };
}

export const fmt = (n) => Number(n).toLocaleString();

/** n bytes, readable: 812 B, 3.4 KB, 1.2 MB. */
export const bytes = (n) => (n < 1024 ? `${n} B` : n < 1048576 ? `${(n / 1024).toFixed(1)} KB` : `${(n / 1048576).toFixed(1)} MB`);

/**
 * The value an edited cell's text stores, by the column's declared type, the
 * way C SQLite's type affinity would read the text: a number for a column of
 * INTEGER, REAL or NUMERIC affinity (or none) when the text is one, the text
 * itself otherwise. Text in single quotes is always text, quotes removed:
 * '42' stores the string 42 in any column.
 */
export function typed(text, type) {
	if (text === null) return null;
	const q = /^'(.*)'$/s.exec(text);
	if (q) return q[1].replace(/''/g, "'");
	const t = (type ?? "").toUpperCase();
	if (/CHAR|CLOB|TEXT|BLOB/.test(t)) return text;
	const s = text.trim();
	if (/^[+-]?\d+$/.test(s)) {
		const n = BigInt(s);
		return n >= BigInt(Number.MIN_SAFE_INTEGER) && n <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(n) : n;
	}
	if (/^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$/.test(s)) return Number(s);
	return text;
}

/** Rows as CSV text, RFC 4180: a header line, then one line per row. */
export function toCSV(columns, rows) {
	const cell = (v) => {
		if (v === null || v === undefined) return "";
		if (v instanceof Uint8Array) v = `x'${Array.from(v, (b) => b.toString(16).padStart(2, "0")).join("")}'`;
		const s = String(v);
		return /[",\r\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
	};
	return [columns, ...rows].map((r) => r.map(cell).join(",")).join("\r\n") + "\r\n";
}

/** Rows as a JSON array of objects; a BLOB as hex, a BigInt as its decimal. */
export function toJSON(columns, rows) {
	const val = (v) => (v instanceof Uint8Array ? Array.from(v, (b) => b.toString(16).padStart(2, "0")).join("") : typeof v === "bigint" ? v.toString() : v);
	return JSON.stringify(rows.map((r) => Object.fromEntries(columns.map((c, i) => [c, val(r[i])]))), null, 2);
}

/** Downloads text (or bytes) as a file named name. */
export function download(name, data, type = "application/octet-stream") {
	const a = document.createElement("a");
	a.href = URL.createObjectURL(new Blob([data], { type }));
	a.download = name;
	a.click();
	setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}
