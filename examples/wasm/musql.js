// The js/wasm JIT's host side (internal/jit/exec_js.go). Call
// musqlJIT(go) before instantiating the Go module with go.importObject.
//
// A kernel module imports the Go instance's memory, so it reads column
// blocks in place; it lands in a JS array and call_kernel runs
// it by slot, once per segment. Compilation is synchronous, which needs a
// Web Worker in a browser (Node has no such limit).
const utf8 = new TextDecoder();

// Node's real fs: bigint mtime keeps nanoseconds.
const statSync = (path) => {
	try {
		const st = globalThis.fs.statSync(path, { bigint: true });
		return [Number(st.size), st.mtimeNs];
	} catch {
		return null;
	}
};

globalThis.musqlJIT = (go, { enabled = true } = {}) => {
	// Kernels by slot. A plain array: nothing calls them through a wasm table.
	const kernels = [];
	go.importObject.musqljit = {
		compile_kernel(ptr, len) {
			if (!enabled) return -1;
			try {
				const mem = go._inst.exports.mem;
				const bytes = new Uint8Array(mem.buffer, ptr, len).slice();
				const inst = new WebAssembly.Instance(new WebAssembly.Module(bytes), { env: { mem } });
				kernels.push(inst.exports.f);
				return kernels.length - 1;
			} catch (e) {
				return -1;
			}
		},
		// The engine's per-statement change check (internal/fsstamp): size
		// and mtime in ns, synchronously, instead of an async fs.stat.
		stat_stamp(ptr, len, out) {
			const mem = go._inst.exports.mem;
			const path = utf8.decode(new Uint8Array(mem.buffer, ptr, len));
			const st = globalThis.fs.statStamp ? globalThis.fs.statStamp(path) : statSync(path);
			if (!st) return 0;
			const dv = new DataView(mem.buffer);
			dv.setBigInt64(out, BigInt(st[0]), true);
			dv.setBigInt64(out + 8, st[1], true);
			return 1;
		},
		// The held-descriptor stamp (internal/fsstamp OfFile): size, mtime in
		// ns and link count of an open file, so a commit need not stat its path.
		fstat_stamp(fd, out) {
			let st;
			if (globalThis.fs.fstatStamp) {
				st = globalThis.fs.fstatStamp(fd);
			} else {
				try {
					const s = globalThis.fs.fstatSync(fd, { bigint: true });
					st = [Number(s.size), s.mtimeNs, Number(s.nlink)];
				} catch {
					st = null;
				}
			}
			if (!st) return 0;
			const dv = new DataView(go._inst.exports.mem.buffer);
			dv.setBigInt64(out, BigInt(st[0]), true);
			dv.setBigInt64(out + 8, BigInt(st[1]), true);
			dv.setBigInt64(out + 16, BigInt(st[2]), true);
			return 1;
		},
		// The commit path's positioned read, positioned write and fsync
		// (internal/fsio): memfs's own when it is the filesystem, Node's
		// otherwise. -1 on any failure.
		file_pread(fd, ptr, n, off) {
			const dst = new Uint8Array(go._inst.exports.mem.buffer, ptr, n);
			try {
				return globalThis.fs.preadSync ? globalThis.fs.preadSync(fd, dst, Number(off)) : globalThis.fs.readSync(fd, dst, 0, n, Number(off));
			} catch {
				return -1;
			}
		},
		file_pwrite(fd, ptr, n, off) {
			const src = new Uint8Array(go._inst.exports.mem.buffer, ptr, n);
			try {
				return globalThis.fs.pwriteSync ? globalThis.fs.pwriteSync(fd, src, Number(off)) : globalThis.fs.writeSync(fd, src, 0, n, Number(off));
			} catch {
				return -1;
			}
		},
		file_fsync(fd) {
			try {
				if (globalThis.fs.fsyncSync) {
					const r = globalThis.fs.fsyncSync(fd);
					return typeof r === "number" ? r : 0;
				}
				return 0;
			} catch {
				return -1;
			}
		},
		call_kernel(slot, args) {
			kernels[slot](args);
		},
	};
};

// musqlQueryFast returns query(sql, args) over the module's own exports
// (musql_inbuf, musql_query, musql_outbuf in examples/wasm/export.go): the
// statement and arguments are written into Go's memory and the JSON answer read
// back, with no syscall/js call in between. It answers exactly what
// musql.querySync does; null when the module has no such exports.
globalThis.musqlQueryFast = function musqlQueryFast(instance) {
	const x = instance.exports;
	if (!x.musql_query) return null;
	const enc = new TextEncoder(), dec = new TextDecoder();
	return (sql, args = []) => {
		const parts = [enc.encode(sql)];
		let n = parts[0].length;
		for (const a of args) {
			if (a === null || a === undefined) { parts.push(new Uint8Array([0])); n += 1; continue; }
			if (typeof a === "string") {
				const s = enc.encode(a), h = new Uint8Array(5);
				h[0] = 3; new DataView(h.buffer).setUint32(1, s.length, true);
				parts.push(h, s); n += 5 + s.length; continue;
			}
			const h = new Uint8Array(9), v = new DataView(h.buffer);
			const num = typeof a === "boolean" ? Number(a) : a;
			if (typeof num === "bigint") { h[0] = 1; v.setBigInt64(1, num, true); }
			else if (Number.isInteger(num)) { h[0] = 1; v.setBigInt64(1, BigInt(num), true); }
			else { h[0] = 2; v.setFloat64(1, num, true); }
			parts.push(h); n += 9;
		}
		const ptr = x.musql_inbuf(n);
		let mem = new Uint8Array(x.mem ? x.mem.buffer : instance.exports.mem.buffer);
		let at = ptr;
		for (const p of parts) { mem.set(p, at); at += p.length; }
		const outLen = x.musql_query(parts[0].length, n - parts[0].length);
		mem = new Uint8Array(instance.exports.mem.buffer); // memory may have grown
		const out = x.musql_outbuf();
		return dec.decode(mem.subarray(out, out + outLen));
	};
};
