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
		call_kernel(slot, args) {
			kernels[slot](args);
		},
	};
};
