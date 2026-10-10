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

// encodeArgs is the wire format wasm/main.go reads: a tag, then the value.
function encodeArgs(args) {
	const parts = [];
	let n = 0;
	for (const a of args) {
		if (a === null || a === undefined) {
			parts.push(Uint8Array.of(0));
			n += 1;
			continue;
		}
		if (typeof a === "string" || bytesOf(a)) {
			const body = typeof a === "string" ? enc.encode(a) : bytesOf(a);
			const h = new Uint8Array(5);
			h[0] = typeof a === "string" ? 3 : 4;
			new DataView(h.buffer).setUint32(1, body.length, true);
			parts.push(h, body);
			n += 5 + body.length;
			continue;
		}
		const h = new Uint8Array(9);
		const dv = new DataView(h.buffer);
		if (typeof a === "bigint") {
			h[0] = 1;
			dv.setBigInt64(1, a, true);
		} else if (typeof a === "boolean") {
			h[0] = 1;
			dv.setBigInt64(1, a ? 1n : 0n, true);
		} else if (typeof a === "number") {
			if (Number.isInteger(a) && Number.isSafeInteger(a)) {
				h[0] = 1;
				dv.setBigInt64(1, BigInt(a), true);
			} else {
				h[0] = 2;
				dv.setFloat64(1, a, true);
			}
		} else {
			throw new TypeError(`musql: cannot bind a ${typeof a}`);
		}
		parts.push(h);
		n += 9;
	}
	return { parts, n };
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
		const s = enc.encode(sql);
		const { parts, n } = encodeArgs(params);
		const ptr = x.musql_inbuf(s.length + n);
		let mem = new Uint8Array(x.mem.buffer);
		mem.set(s, ptr);
		let at = ptr + s.length;
		for (const p of parts) {
			mem.set(p, at);
			at += p.length;
		}
		const len = x[fn](this.#h, s.length, n);
		mem = new Uint8Array(x.mem.buffer); // the call may have grown memory
		const out = x.musql_outbuf();
		const res = JSON.parse(dec.decode(mem.subarray(out, out + len)), revive);
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

	/** Returns { columns, rows }, each row an array in column order. */
	query(sql, ...params) {
		return this.#call("musql_query", sql, params);
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
 *   data: a database file to start from (an earlier export()); jit: force the
 *   JIT on or off (default: on in Node and Web Workers, off on a page's main thread)
 */
export async function open(name = ":memory:", options = {}) {
	const jit = options.jit ?? (isNode || inWorker);
	const x = await start(jit);
	const path = name === ":memory:" || name === "" ? `/memory-${++anon}.musq` : `/${name.replace(/^\/+/, "")}`;
	if (options.data !== undefined) {
		const data = bytesOf(options.data);
		if (!data) throw new TypeError("musql: data must be a Uint8Array or ArrayBuffer");
		globalThis.fs.writeFileSync(path, data);
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
