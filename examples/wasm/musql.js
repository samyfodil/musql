// The js/wasm JIT's host side (internal/jit/exec_js.go). Call
// musqlJIT(go) before instantiating the Go module with go.importObject.
//
// A kernel module imports the Go instance's memory, so it reads column
// blocks in place; it lands in a WebAssembly.Table and call_kernel runs
// it by slot, once per segment. Compilation is synchronous, which needs a
// Web Worker in a browser (Node has no such limit).
globalThis.musqlJIT = (go, { enabled = true } = {}) => {
	const table = new WebAssembly.Table({ initial: 0, element: "anyfunc" });
	go.importObject.musqljit = {
		compile_kernel(ptr, len) {
			if (!enabled) return -1;
			try {
				const mem = go._inst.exports.mem;
				const bytes = new Uint8Array(mem.buffer, ptr, len).slice();
				const inst = new WebAssembly.Instance(new WebAssembly.Module(bytes), { env: { mem } });
				const slot = table.grow(1);
				table.set(slot, inst.exports.f);
				return slot;
			} catch (e) {
				return -1;
			}
		},
		call_kernel(slot, args) {
			table.get(slot)(args);
		},
	};
};
