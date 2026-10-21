// musql in JavaScript: the WebAssembly build of musql (wasm/main.go) over an
// in-memory filesystem, for Node and browsers.
//
//   import { open } from "@samyfodil/musql";
//   const db = await open("app.db");
//   db.exec("CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT)");
//   db.run("INSERT INTO t(name) VALUES (?)", "ada");
//   db.all("SELECT * FROM t");            // [{ id: 1, name: "ada" }]
//   const bytes = db.export();             // the database file, to keep
//   const again = await open("copy.db", { data: bytes });
//   const sqlite = db.exportSQLite();      // the same database as a SQLite file
//   const back = await open("c.db", { data: sqlite }); // SQLite files convert on open
//
// Every database lives in memory, in this page or process; export() and the
// data option move one in and out. The JIT compiles query kernels to wasm at
// run time, which a browser allows only in a Web Worker: on a page's main
// thread musql runs without it (same answers, slower).
import "./memfs.js";
import "./wasm_exec.js";
import "./glue.js";

const enc = new TextEncoder();
const dec = new TextDecoder();
const isNode = typeof process !== "undefined" && !!process.versions?.node;
const inWorker = typeof WorkerGlobalScope !== "undefined" && globalThis instanceof WorkerGlobalScope;

let runtime;

// start instantiates the module once per page or process.
function start(jit) {
	runtime ??= (async () => {
		const url = new URL("./musql.wasm", import.meta.url);
		const go = new Go();
		go.env = { TMPDIR: "/tmp" };
		globalThis.musqlJIT(go, { enabled: jit });
		const ready = new Promise((resolve) => { globalThis.__musqlReady = resolve; });
		let instance;
		if (isNode) {
			const { readFile } = await import("node:fs/promises");
			({ instance } = await WebAssembly.instantiate(await readFile(url), go.importObject));
		} else {
			({ instance } = await WebAssembly.instantiateStreaming(fetch(url), go.importObject));
		}
		go.run(instance);
		await ready;
		return instance.exports;
	})();
	return runtime;
}

function bytesOf(v) {
	if (v instanceof Uint8Array) return v;
	if (v instanceof ArrayBuffer) return new Uint8Array(v);
	if (ArrayBuffer.isView(v)) return new Uint8Array(v.buffer, v.byteOffset, v.byteLength);
	return null;
}

// argBytes is how many bytes writeArgs writes for args.
function argBytes(args) {
	let n = 0;
	for (const a of args) {
		if (a === null || a === undefined) n += 1;
		else if (typeof a === "string") n += 5 + 3 * a.length; // UTF-8's most per UTF-16 unit
		else if (bytesOf(a)) n += 5 + bytesOf(a).length;
		else n += 9;
	}
	return n;
}

// writeArgs writes args at mem[at] in the wire format wasm/main.go reads (a
// tag, then the value) and returns how many bytes it wrote.
function writeArgs(mem, at, args) {
	const dv = new DataView(mem.buffer, mem.byteOffset, mem.byteLength);
	const start = at;
	for (const a of args) {
		if (a === null || a === undefined) {
			mem[at++] = 0;
		} else if (typeof a === "string") {
			const { written } = enc.encodeInto(a, mem.subarray(at + 5));
			mem[at] = 3;
			dv.setUint32(at + 1, written, true);
			at += 5 + written;
		} else if (bytesOf(a)) {
			const b = bytesOf(a);
			mem[at] = 4;
			dv.setUint32(at + 1, b.length, true);
			mem.set(b, at + 5);
			at += 5 + b.length;
		} else if (typeof a === "bigint") {
			mem[at] = 1;
			dv.setBigInt64(at + 1, a, true);
			at += 9;
		} else if (typeof a === "boolean") {
			mem[at] = 1;
			dv.setBigInt64(at + 1, a ? 1n : 0n, true);
			at += 9;
		} else if (typeof a === "number") {
			if (Number.isSafeInteger(a)) {
				mem[at] = 1;
				dv.setBigInt64(at + 1, BigInt(a), true);
			} else {
				mem[at] = 2;
				dv.setFloat64(at + 1, a, true);
			}
			at += 9;
		} else {
			throw new TypeError(`musql: cannot bind a ${typeof a}`);
		}
	}
	return at - start;
}

// sqlBytes caches each statement text's UTF-8, which a page sends again and
// again; bounded, so a stream of distinct texts cannot grow it without end.
const sqlBytes = new Map();
function encodeSQL(sql) {
	let b = sqlBytes.get(sql);
	if (b === undefined) {
		b = enc.encode(sql);
		if (sqlBytes.size >= 256) sqlBytes.clear();
		sqlBytes.set(sql, b);
	}
	return b;
}

// tagged reports whether a result's text holds a value only revive restores:
// most results hold none, and a reviver costs a call per value.
function tagged(bytes) {
	const text = dec.decode(bytes);
	return { text, revive: text.includes('{"i":"') || text.includes('{"b":"') || text.includes('{"f":"') };
}

function fromBase64(s) {
	if (isNode) return new Uint8Array(Buffer.from(s, "base64"));
	const bin = atob(s);
	const out = new Uint8Array(bin.length);
	for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
	return out;
}

// revive turns the module's tagged values back into JavaScript ones.
function revive(_, v) {
	if (v && typeof v === "object" && !Array.isArray(v)) {
		if (typeof v.i === "string") return BigInt(v.i);
		if (typeof v.b === "string") return fromBase64(v.b);
		if (typeof v.f === "string") return Number(v.f);
	}
	return v;
}

const SQLITE_HEADER = enc.encode("SQLite format 3\0");

function isSQLite(b) {
	return b.length >= 16 && SQLITE_HEADER.every((c, i) => b[i] === c);
}

// convert runs the module's converter over two in-memory paths.
function convert(x, toSQLite, src, dst) {
	const s = enc.encode(src), d = enc.encode(dst);
	const ptr = x.musql_inbuf(s.length + d.length);
	const mem = new Uint8Array(x.mem.buffer);
	mem.set(s, ptr);
	mem.set(d, ptr + s.length);
	const len = x.musql_convert(toSQLite ? 1 : 0, s.length, d.length);
	const out = x.musql_outbuf();
	const res = JSON.parse(dec.decode(new Uint8Array(x.mem.buffer).subarray(out, out + len)));
	if (res.error !== undefined) throw new Error(res.error);
}

const unlink = (p) => globalThis.fs.unlink(p, () => {});

class Database {
	#x;
	#h;
	#path;

	constructor(x, h, path) {
		this.#x = x;
		this.#h = h;
		this.#path = path;
	}

	// #call writes sql and its arguments into the module and runs fn.
	#call(fn, sql, params) {
		if (this.#h === 0) throw new Error("musql: the database is closed");
		const x = this.#x;
		const s = encodeSQL(sql);
		const ptr = x.musql_inbuf(s.length + argBytes(params));
		let mem = new Uint8Array(x.mem.buffer);
		mem.set(s, ptr);
		const n = writeArgs(mem, ptr + s.length, params);
		const len = x[fn](this.#h, s.length, n);
		mem = new Uint8Array(x.mem.buffer); // the call may have grown memory
		const out = x.musql_outbuf();
		const { text, revive: slow } = tagged(mem.subarray(out, out + len));
		const res = slow ? JSON.parse(text, revive) : JSON.parse(text);
		if (res.error !== undefined) throw new Error(res.error);
		return res;
	}

	/** Runs one statement; returns { changes, lastInsertRowid }. */
	run(sql, ...params) {
		return this.#call("musql_exec", sql, params);
	}

	/** Runs SQL that returns no rows, for example a schema. */
	exec(sql) {
		this.#call("musql_exec", sql, []);
		return this;
	}

	/**
	 * Returns { columns, types, rows }: each row an array in column order, and
	 * types each column's declared type ("" where it has none).
	 */
	query(sql, ...params) {
		return this.#call("musql_query", sql, params);
	}

	/**
	 * Splits a script into its statements, as the engine does: a semicolon in a
	 * string, a comment or a trigger body ends nothing.
	 */
	split(sql) {
		const x = this.#x;
		const s = enc.encode(sql);
		new Uint8Array(x.mem.buffer).set(s, x.musql_inbuf(s.length));
		const len = x.musql_split(s.length);
		const out = x.musql_outbuf();
		const res = JSON.parse(dec.decode(new Uint8Array(x.mem.buffer).subarray(out, out + len)));
		if (res.error !== undefined) throw new Error(res.error);
		return res.statements;
	}

	/** Returns every row as an object keyed by column name. */
	all(sql, ...params) {
		const { columns, rows } = this.query(sql, ...params);
		return rows.map((r) => Object.fromEntries(columns.map((c, i) => [c, r[i]])));
	}

	/** Returns the first row as an object, or undefined. */
	get(sql, ...params) {
		return this.all(sql, ...params)[0];
	}

	/**
	 * Returns the database file's bytes. It compacts the database first
	 * (VACUUM), so the bytes are the whole database in one file.
	 */
	export() {
		this.exec("VACUUM");
		return globalThis.fs.readFileSync(this.#path);
	}

	/** Returns the database as a C SQLite file's bytes, for sqlite3 and every other SQLite tool. */
	exportSQLite() {
		this.exec("VACUUM");
		const tmp = `${this.#path}.export.sqlite`;
		convert(this.#x, true, this.#path, tmp);
		try {
			return globalThis.fs.readFileSync(tmp);
		} finally {
			unlink(tmp);
		}
	}

	/** Closes the database. Its file stays in memory until the page or process ends. */
	close() {
		if (this.#h !== 0) {
			this.#x.musql_close(this.#h);
			this.#h = 0;
		}
	}
}

let anon = 0;

/**
 * Opens (or creates) an in-memory database.
 *
 * @param {string} [name] a file name; ":memory:" or none gives a fresh, private one
 * @param {{data?: Uint8Array|ArrayBuffer, jit?: boolean}} [options]
 *   data: a database file to start from, musql's (an earlier export()) or a
 *   C SQLite file, which is converted on the way in; jit: force the
 *   JIT on or off (default: on in Node and Web Workers, off on a page's main thread)
 */
export async function open(name = ":memory:", options = {}) {
	const jit = options.jit ?? (isNode || inWorker);
	const x = await start(jit);
	const path = name === ":memory:" || name === "" ? `/memory-${++anon}.musq` : `/${name.replace(/^\/+/, "")}`;
	if (options.data !== undefined) {
		const data = bytesOf(options.data);
		if (!data) throw new TypeError("musql: data must be a Uint8Array or ArrayBuffer");
		if (isSQLite(data)) {
			const tmp = `${path}.import.sqlite`;
			globalThis.fs.writeFileSync(tmp, data);
			try {
				convert(x, false, tmp, path);
			} finally {
				unlink(tmp);
			}
		} else {
			globalThis.fs.writeFileSync(path, data);
		}
	}
	const p = enc.encode(path);
	const ptr = x.musql_inbuf(p.length);
	new Uint8Array(x.mem.buffer).set(p, ptr);
	const h = x.musql_open(p.length);
	if (h < 0) {
		const out = x.musql_outbuf();
		throw new Error(JSON.parse(dec.decode(new Uint8Array(x.mem.buffer).subarray(out, out - h - 1))).error);
	}
	return new Database(x, h, path);
}
