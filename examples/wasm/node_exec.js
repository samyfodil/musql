// go test -exec runner for GOOS=js: Go's own wasm_exec_node.js plus the
// musqljit imports. MUSQL_JIT=0 makes every kernel compile decline.
"use strict";
const path = require("path");
const goroot = require("child_process").execSync("go env GOROOT").toString().trim();
globalThis.fs = require("fs");
require(path.join(__dirname, "musql.js"));
require(path.join(goroot, "lib/wasm/wasm_exec.js"));
// wasm_exec_node.js constructs Go and instantiates as it loads, so the
// imports go in through a subclass. Its own require("./wasm_exec") is the
// cached module above and does not redefine Go.
const Real = globalThis.Go;
globalThis.Go = class extends Real {
	constructor() {
		super();
		musqlJIT(this, { enabled: process.env.MUSQL_JIT !== "0" });
	}
};
require(path.join(goroot, "lib/wasm/wasm_exec_node.js"));
