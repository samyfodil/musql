// Runs the demo's Go module under Node over memfs, in one mode per process:
//   node examples/wasm/node_demo.js musql.wasm vdbe|go|jit [rows]
// vdbe is the plain VDBE (MUSQL_JIT=0), go keeps the columnar paths but the
// host declines every kernel so Go's own loops run, jit runs the wasm kernels.
"use strict";
const path = require("path");
const goroot = require("child_process").execSync("go env GOROOT").toString().trim();
require(path.join(__dirname, "memfs.js"));
require(path.join(__dirname, "musqljit.js"));
require(path.join(goroot, "lib/wasm/wasm_exec.js"));
(async () => {
	const [wasm, mode = "jit", rows = "100000"] = process.argv.slice(2);
	const go = new Go();
	go.env = { TMPDIR: "/tmp", MUSQL_JIT: mode === "vdbe" ? "0" : "1" };
	musqlJIT(go, { enabled: mode === "jit" });
	const ready = new Promise((r) => { globalThis.musqlReady = r; });
	const { instance } = await WebAssembly.instantiate(require("fs").readFileSync(wasm), go.importObject);
	go.run(instance);
	await ready;
	console.error(mode, "vector:", musql.vector, await musql.seed(Number(rows)));
	console.log(JSON.stringify({ mode, results: await musql.bench() }));
	process.exit(0);
})().catch((e) => { console.error(e); process.exit(1); });
