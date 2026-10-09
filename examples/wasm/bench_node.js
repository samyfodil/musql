// musql's wasm build under Node: the race page's load, point lookups, single-row
// writes, and aggregates before and after a write, through the module's own
// exports as the page's worker calls them. No Turso here -- race.html is the
// comparison; this is for timing musql alone, repeatably, from a terminal.
//
//   GOOS=js GOARCH=wasm go build -o examples/wasm/musql.wasm ./examples/wasm
//   node examples/wasm/bench_node.js [rows=100000] [iterations=2000]
"use strict";
const path = require("path");
const goroot = require("child_process").execSync("go env GOROOT").toString().trim();
require(path.join(__dirname, "memfs.js"));
require(path.join(__dirname, "musql.js"));
require(path.join(goroot, "lib/wasm/wasm_exec.js"));

const rows = Number(process.argv[2] || 100000);
const iters = Number(process.argv[3] || 2000);

// The race page's seed (race.js seedSQL), in the same 200k-row chunks.
function seedSQL(n) {
	const out = [
		"CREATE TABLE t (id INTEGER PRIMARY KEY, sec INTEGER, k INTEGER, v INTEGER, bid INTEGER, payload TEXT)",
		"CREATE TABLE b (id INTEGER PRIMARY KEY, label TEXT)",
		"CREATE INDEX idx_t_sec ON t(sec)",
	];
	for (let lo = 0; lo < n; lo += 200000) {
		const hi = Math.min(lo + 200000, n);
		out.push(`WITH RECURSIVE c(i) AS (SELECT ${lo} UNION ALL SELECT i + 1 FROM c WHERE i < ${hi - 1}) INSERT INTO b SELECT i, 'label-' || i FROM c`);
		out.push(`WITH RECURSIVE c(i) AS (SELECT ${lo + 1} UNION ALL SELECT i + 1 FROM c WHERE i < ${hi}) INSERT INTO t SELECT i, (i * 7919) % ${n}, (i * 31) % 10, (i * 104729) % 1000000, 1 + (i * 13) % ${n}, 'row-' || i || '-payload' FROM c`);
	}
	out.push("VACUUM");
	return out;
}

(async () => {
	const go = new Go();
	go.env = { TMPDIR: "/tmp", MUSQL_JIT: "1" };
	musqlJIT(go, { enabled: true });
	const ready = new Promise((r) => { globalThis.musqlReady = r; });
	const { instance } = await WebAssembly.instantiate(require("fs").readFileSync(path.join(__dirname, "musql.wasm")), go.importObject);
	go.run(instance);
	await ready;
	const q = musqlQueryFast(instance);
	const run = (sql, args = []) => {
		const r = JSON.parse(q(sql, args));
		if (r.error) throw new Error(`${sql}: ${r.error}`);
		return r.rows;
	};
	const time = (label, sql, arg, n = iters) => {
		for (let i = 0; i < Math.min(n, 50); i++) run(sql, arg(i));
		const t0 = performance.now();
		for (let i = 0; i < n; i++) run(sql, arg(i));
		console.log(`${label.padEnd(44)} ${((performance.now() - t0) * 1000 / n).toFixed(1).padStart(9)} us`);
	};

	let t0 = performance.now();
	for (const sql of seedSQL(rows)) await musql.exec(sql);
	console.log(`${`load ${rows} rows (+ VACUUM)`.padEnd(44)} ${((performance.now() - t0)).toFixed(0).padStart(9)} ms`);

	const id = (i) => [1 + (i * 37) % rows];
	console.log("-- reads");
	time("rowid point lookup", "SELECT sec FROM t WHERE id = ?", id);
	time("secondary-index eq", "SELECT count(*) FROM t WHERE sec = ?", id);
	time("count(*) WHERE v > ?", "SELECT count(*) FROM t WHERE v > ?", () => [500000], 200);
	time("count(*), sum(v)", "SELECT count(*), sum(v) FROM t", () => [], 200);
	time("max(id)", "SELECT max(id) FROM b", () => []);

	console.log("-- single-row writes, each its own commit");
	time("UPDATE by rowid", "UPDATE t SET v = v + 1 WHERE id = ?", id);
	time("UPDATE by indexed column", "UPDATE t SET v = v + 1 WHERE sec = ?", id);
	time("INSERT one row", "INSERT INTO b(label) VALUES(?)", (i) => [`x${i}`]);
	time("DELETE the last row", "DELETE FROM b WHERE id = (SELECT max(id) FROM b)", () => []);

	console.log("-- reads after the writes (the table now has a delta)");
	time("count(*) WHERE v > ?", "SELECT count(*) FROM t WHERE v > ?", () => [500000], 50);
	time("count(*), sum(v)", "SELECT count(*), sum(v) FROM t", () => [], 50);
	time("count(*), max(id) FROM b", "SELECT count(*), max(id) FROM b", () => [], 50);
	console.log("state", JSON.stringify([...run("SELECT count(*), sum(v) FROM t"), ...run("SELECT count(*), max(id) FROM b")]));
	process.exit(0);
})().catch((e) => { console.error(e); process.exit(1); });
