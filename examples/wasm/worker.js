// Runs musql (musql.wasm) in a Web Worker: synchronous wasm compiles, which
// the JIT needs, are allowed here and not on the main thread.
importScripts("memfs.js", "musql.js", "wasm_exec.js");

const ready = new Promise((resolve) => { globalThis.musqlReady = resolve; });
(async () => {
	// vdbe: the plain VDBE; go: columnar paths, every kernel declined; jit: all on.
	const mode = new URLSearchParams(location.search).get("mode") || "jit";
	const go = new Go();
	go.env = { TMPDIR: "/tmp", MUSQL_JIT: mode === "vdbe" ? "0" : "1" };
	musqlJIT(go, { enabled: mode === "jit" });
	const { instance } = await WebAssembly.instantiateStreaming(fetch("musql.wasm"), go.importObject);
	go.run(instance);
	await ready;
	postMessage({ ready: true, mode, vector: musql.vector });
})().catch((e) => postMessage({ error: String(e) }));

onmessage = async ({ data: { id, op, args = [] } }) => {
	await ready;
	try {
		if (op === "load") {
			fs.writeFileSync("/bench.musq", args[0]);
			await musql.open();
			postMessage({ id, result: "loaded" });
			return;
		}
		if (op === "save") {
			postMessage({ id, result: fs.readFileSync("/bench.musq") });
			return;
		}
		postMessage({ id, result: await musql[op](...args) });
	} catch (e) {
		postMessage({ id, error: String(e) });
	}
};
