// musql vs Turso in the browser. Each engine runs in its own worker
// (worker.js, turso-worker.js) behind the same message protocol.
"use strict";

const $ = (id) => document.getElementById(id);
const ENGINES = ["musql", "turso"];

function spawn(url, opts) {
	const w = new Worker(url, opts);
	const pending = new Map();
	let seq = 0;
	const ready = new Promise((resolve, reject) => {
		w.onmessage = ({ data }) => {
			if (data.ready) return resolve(data);
			if (data.error && data.id === undefined) return reject(new Error(data.error));
			const p = pending.get(data.id);
			pending.delete(data.id);
			data.error ? p.reject(new Error(data.error)) : p.resolve(data.result);
		};
	});
	const call = (op, ...args) => new Promise((resolve, reject) => {
		pending.set(++seq, { resolve, reject });
		w.postMessage({ id: seq, op, args });
	});
	return { ready, call };
}

const eng = {
	musql: spawn("worker.js?mode=jit"),
	turso: spawn("turso-worker.js", { type: "module" }),
};

// A seeded generator, so both engines get the same bound values.
function rng(seed) {
	return () => {
		seed |= 0; seed = (seed + 0x6d2b79f5) | 0;
		let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
		t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
		return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
	};
}
const int = (r, n) => Math.floor(r() * n);

// The harness's benchmark queries (compat-harness/bench_columnar_vs_c_test.go).
const QUERIES = [
	["count(*) WHERE v > ?", "SELECT count(*) FROM t WHERE v > ?", (r) => [int(r, 1000000)]],
	["count(*) WHERE v > ? AND k <> ?", "SELECT count(*) FROM t WHERE v > ? AND k <> ?", (r) => [int(r, 1000000), int(r, 10)]],
	["rowid point lookup", "SELECT sec FROM t WHERE id = ?", (r, n) => [1 + int(r, n)]],
	["secondary-index eq", "SELECT count(*) FROM t WHERE sec = ?", (r, n) => [int(r, n)]],
	["indexed equi-join", "SELECT count(*) FROM t JOIN b ON t.bid = b.id WHERE t.sec = ?", (r, n) => [int(r, n)]],
	["sum over a filter", "SELECT sum(v) FROM t WHERE v > ?", (r) => [int(r, 1000000)]],
	["aggregate GROUP BY", "SELECT k, count(*), sum(v) FROM t GROUP BY k ORDER BY k", () => []],
	["ORDER BY v DESC LIMIT 20", "SELECT id, v FROM t ORDER BY v DESC, id DESC LIMIT 20", () => []],
	["count(*) whole table", "SELECT count(*) FROM t", () => []],
	["OR predicate", "SELECT count(*) FROM t WHERE v < ? OR k = ?", (r) => [int(r, 1000000), int(r, 10)]],
	["arithmetic over every row", "SELECT sum(v * 3 + k - id) FROM t", () => []],
	["filtered GROUP BY", "SELECT k, count(*), sum(v) FROM t WHERE v > ? GROUP BY k ORDER BY k", (r) => [int(r, 1000000)]],
	["correlated EXISTS", "SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.bid AND b.id < ?)", (r, n) => [int(r, n)]],
];

function seedSQL(n) {
	return [
		"DROP TABLE IF EXISTS t", "DROP TABLE IF EXISTS b",
		"CREATE TABLE t (id INTEGER PRIMARY KEY, sec INTEGER, k INTEGER, v INTEGER, bid INTEGER, payload TEXT)",
		"CREATE TABLE b (id INTEGER PRIMARY KEY, label TEXT)",
		"CREATE INDEX idx_t_sec ON t(sec)",
		`WITH RECURSIVE c(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM c WHERE i < ${n - 1}) INSERT INTO b SELECT i, 'label-' || i FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < ${n}) INSERT INTO t SELECT i, (i * 7919) % ${n}, (i * 31) % 10, (i * 104729) % 1000000, 1 + (i * 13) % ${n}, 'row-' || i || '-payload' FROM c`,
	];
}

// Rows in a form both engines agree on: Turso may hand back a BigInt.
const norm = (rows) => JSON.stringify(rows, (_, v) => (typeof v === "bigint" ? Number(v) : v));

let rows = 0;
const score = { musql: 0, turso: 0 };
const busy = (on) => ["seed", "race", "duel", "custom"].forEach((b) => ($(b).disabled = on || (b !== "seed" && !rows)));

function setScore() {
	for (const e of ENGINES) $(`${e}-score`).textContent = score[e];
}

function resetBoard() {
	const body = $("board").tBodies[0];
	body.innerHTML = "";
	for (const [name, sql] of QUERIES) {
		const tr = body.insertRow();
		tr.title = sql;
		tr.insertCell().textContent = name;
		for (let i = 0; i < 5; i++) tr.insertCell().textContent = "·";
	}
	for (const e of ENGINES) {
		$(`${e}-bar`).style.width = "0";
		$(`${e}-total`).textContent = "";
		document.querySelector(`.lane[data-e=${e}]`).classList.remove("winner");
	}
}

function fill(i, t) {
	const tr = $("board").tBodies[0].rows[i];
	const c = tr.cells;
	c[1].textContent = t.musql.toFixed(3);
	c[2].textContent = t.turso.toFixed(3);
	const w = t.musql <= t.turso ? "musql" : "turso";
	const ratio = Math.max(t.musql, t.turso) / Math.max(Math.min(t.musql, t.turso), 1e-6);
	c[3].textContent = w === "musql" ? "musql" : "Turso";
	c[3].className = c[4].className = `w-${w}`;
	c[4].textContent = `${ratio < 10 ? ratio.toFixed(1) : Math.round(ratio)}×`;
	c[5].textContent = t.same ? "✓" : "✗ differs";
	c[5].className = t.same ? "ok" : "bad";
	return t.same ? w : null;
}

const median = (xs) => { const s = [...xs].sort((a, b) => a - b); return s[s.length >> 1]; };

// runQuery runs one query `runs` times on one engine with the shared bound
// values and returns the per-run times and the first run's rows.
async function runQuery(e, qi, runs, onRun) {
	const [, sql, gen] = QUERIES[qi];
	const r = rng(1000 + qi);
	const times = [];
	let first;
	for (let k = 0; k < runs; k++) {
		const res = await eng[e].call("query", sql, gen(r, rows));
		if (k === 0) first = norm(res.rows);
		times.push(res.ms);
		onRun?.();
	}
	return { times, first };
}

async function seed() {
	busy(true);
	const n = Number($("rows").value);
	$("banner").textContent = `Loading ${n.toLocaleString()} rows into both…`;
	const load = async (e) => {
		const t = performance.now();
		for (const q of seedSQL(n)) await eng[e].call("exec", q);
		if (e === "musql") await eng.musql.call("exec", "VACUUM");
		const ms = performance.now() - t;
		$(`${e}-status`).textContent = `${n.toLocaleString()} rows loaded in ${(ms / 1000).toFixed(2)} s`;
		return ms;
	};
	try {
		await Promise.all(ENGINES.map(load));
		rows = n;
		resetBoard();
		$("banner").textContent = "Ready. Pick a mode.";
	} catch (e) {
		$("banner").textContent = `Load failed: ${e.message}`;
	}
	busy(false);
}

async function race() {
	busy(true);
	resetBoard();
	const runs = Number($("runs").value);
	const total = QUERIES.length * runs;
	$("banner").textContent = "🏎️ Racing… both engines are running right now.";
	const results = { musql: [], turso: [] };
	const finish = {};
	const t0 = performance.now();
	const lane = async (e) => {
		let done = 0;
		for (let qi = 0; qi < QUERIES.length; qi++) {
			results[e][qi] = await runQuery(e, qi, runs, () => ($(`${e}-bar`).style.width = `${(100 * ++done) / total}%`));
		}
		finish[e] = performance.now() - t0;
		$(`${e}-total`).textContent = `finished in ${(finish[e] / 1000).toFixed(2)} s`;
	};
	try {
		await Promise.all(ENGINES.map(lane));
		const pts = { musql: 0, turso: 0 };
		QUERIES.forEach((_, qi) => {
			const t = {
				musql: median(results.musql[qi].times),
				turso: median(results.turso[qi].times),
				same: results.musql[qi].first === results.turso[qi].first,
			};
			const w = fill(qi, t);
			if (w) pts[w]++;
		});
		const w = finish.musql <= finish.turso ? "musql" : "turso";
		score[w] += 3;
		for (const e of ENGINES) score[e] += pts[e];
		setScore();
		document.querySelector(`.lane[data-e=${w}]`).classList.add("winner");
		const by = Math.max(finish.musql, finish.turso) / Math.min(finish.musql, finish.turso);
		$("banner").textContent = `🏁 ${w === "musql" ? "musql" : "Turso"} crosses the line first, ${by.toFixed(1)}× sooner. Queries won: musql ${pts.musql}, Turso ${pts.turso}.`;
		log(`Race, ${runs} runs/query: ${w} won by ${by.toFixed(1)}× (queries ${pts.musql}–${pts.turso})`);
	} catch (e) {
		$("banner").textContent = `Race failed: ${e.message}`;
	}
	busy(false);
}

async function duel() {
	busy(true);
	resetBoard();
	const runs = Number($("runs").value);
	const pts = { musql: 0, turso: 0 };
	const body = $("board").tBodies[0];
	try {
		for (let qi = 0; qi < QUERIES.length; qi++) {
			body.rows[qi].classList.add("running");
			$("banner").textContent = `⚔️ Round ${qi + 1} of ${QUERIES.length}: ${QUERIES[qi][0]}`;
			// Alternate who goes first, so neither always gets the warmer cache.
			const order = qi % 2 ? ["turso", "musql"] : ["musql", "turso"];
			const res = {};
			for (const e of order) {
				res[e] = await runQuery(e, qi, runs);
				$(`${e}-bar`).style.width = `${(100 * (qi + 1)) / QUERIES.length}%`;
			}
			const w = fill(qi, { musql: median(res.musql.times), turso: median(res.turso.times), same: res.musql.first === res.turso.first });
			body.rows[qi].classList.remove("running");
			if (w) {
				pts[w]++;
				score[w]++;
				setScore();
			}
		}
		const w = pts.musql >= pts.turso ? "musql" : "turso";
		document.querySelector(`.lane[data-e=${w}]`).classList.add("winner");
		$("banner").textContent = `🏆 Duel over: musql ${pts.musql}, Turso ${pts.turso}.`;
		log(`Duel, ${runs} runs/query: musql ${pts.musql} – Turso ${pts.turso}`);
	} catch (e) {
		$("banner").textContent = `Duel failed: ${e.message}`;
	}
	busy(false);
}

// custom runs the query once cold, then 10 times warm, on each engine. The
// first run includes compiling the statement -- and for musql its JIT
// kernels -- so it is reported apart from the warm median.
async function custom() {
	busy(true);
	const sql = $("sql").value;
	const res = {};
	for (const e of ENGINES) {
		try {
			const first = await eng[e].call("query", sql, []);
			const warm = [];
			for (let k = 0; k < 10; k++) warm.push((await eng[e].call("query", sql, [])).ms);
			res[e] = { ...first, first: first.ms, warm: median(warm) };
			$(`${e}-out`).textContent = [`${e} · first ${first.ms.toFixed(3)} ms · warm ${res[e].warm.toFixed(3)} ms`, first.columns.join("\t"),
				...first.rows.slice(0, 50).map((x) => x.map((v) => String(v)).join("\t")), first.rows.length > 50 ? `… ${first.rows.length} rows` : ""].join("\n");
		} catch (err) {
			res[e] = { error: err.message };
			$(`${e}-out`).textContent = `error: ${err.message}`;
		}
	}
	if (res.musql.error || res.turso.error) {
		$("custom-res").textContent = "one side errored";
	} else {
		const same = norm(res.musql.rows) === norm(res.turso.rows);
		const w = res.musql.warm <= res.turso.warm ? "musql" : "Turso";
		const by = Math.max(res.musql.warm, res.turso.warm) / Math.max(Math.min(res.musql.warm, res.turso.warm), 1e-6);
		$("custom-res").textContent = `${same ? "✓ same answer" : "✗ answers differ"} · warm: ${w} faster by ${by.toFixed(1)}×`;
	}
	busy(false);
}

function log(s) {
	const li = document.createElement("li");
	li.textContent = `${new Date().toLocaleTimeString()} · ${rows.toLocaleString()} rows · ${s}`;
	$("history").prepend(li);
	window.raceLog = [...(window.raceLog || []), s];
}

$("seed").onclick = seed;
$("race").onclick = race;
$("duel").onclick = duel;
$("custom").onclick = custom;

Promise.all(ENGINES.map((e) => eng[e].ready)).then(([m]) => {
	$("musql-status").textContent = `ready · JIT kernels: ${m.vector ? "SIMD128" : "scalar"}`;
	$("turso-status").textContent = "ready";
	$("banner").textContent = crossOriginIsolated ? "Both engines are up. Load data to start." : "This page is not cross-origin isolated, so Turso cannot start its threads: serve it with go run ./examples/wasm/serve.";
	busy(false);
	window.raceReady = true;
}, (e) => ($("banner").textContent = `An engine failed to start: ${e.message}`));
