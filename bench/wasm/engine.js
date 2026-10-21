// One engine in a module worker: load, check, time. The worker timing is
// what the engine itself costs; no message crosses during a timed loop.
import { setup, workloads, seeded, N } from "./workloads.js";

const BUDGET = 200; // ms per workload

export async function runEngine(name, db) {
	const out = { name, rows: N, workloads: [] };
	let t = performance.now();
	for (const s of setup) await db.exec(s);
	out.loadMs = performance.now() - t;
	t = performance.now();
	await db.vacuum?.();
	out.vacuumMs = performance.now() - t;

	for (const [label, sql, params] of workloads) {
		const stmt = await db.prepare(sql);
		// The answers for 20 fixed param sets, which the page compares across engines.
		const check = seeded(7), answers = [];
		for (let i = 0; i < 20; i++) answers.push(await stmt.all(params(check)));
		const r = seeded(1);
		let n = 0;
		const start = performance.now();
		let now = start;
		while (now - start < BUDGET) {
			await stmt.all(params(r));
			n++;
			now = performance.now();
		}
		out.workloads.push({ label, us: ((now - start) * 1000) / n, n, answers: JSON.stringify(answers, (k, v) => typeof v === "bigint" ? Number(v) : v) });
	}

	// Writes: single-row inserts in autocommit, then a batch in one transaction.
	await db.exec("CREATE TABLE w(id INTEGER PRIMARY KEY, a INTEGER, b TEXT)");
	const ins = await db.prepare("INSERT INTO w(a, b) VALUES (?, ?)");
	let n = 0;
	t = performance.now();
	while (performance.now() - t < BUDGET) await ins.run(n, "x" + n++);
	out.workloads.push({ label: "INSERT one row (autocommit)", us: ((performance.now() - t) * 1000) / n, n, answers: "" });
	t = performance.now();
	await db.exec("BEGIN");
	for (let i = 0; i < 10000; i++) await ins.run(i, "y" + i);
	await db.exec("COMMIT");
	out.workloads.push({ label: "INSERT 10,000 rows in one transaction", us: (performance.now() - t) * 1000, n: 1, answers: "" });
	return out;
}
