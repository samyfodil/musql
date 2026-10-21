// Runs each engine in its own worker, one after the other, and prints a table.
// window.benchResult holds the raw numbers for a headless runner.
const run = (url) => new Promise((resolve, reject) => {
	const w = new Worker(url, { type: "module" });
	w.onmessage = (e) => { w.terminate(); resolve(e.data); };
	w.onerror = (e) => reject(new Error(e.message));
});

const out = document.getElementById("out");
try {
	const m = await run(new URL("./musql-worker.js", import.meta.url));
	out.textContent = "musql done, running turso...";
	const t = await run(new URL("./turso-worker.js", import.meta.url));
	const fmt = (us) => us >= 1000 ? (us / 1000).toFixed(2) + " ms" : us.toFixed(1) + " µs";
	const lines = [`${m.rows} rows; load musql ${m.loadMs.toFixed(0)} ms (+VACUUM ${m.vacuumMs.toFixed(0)} ms), turso ${t.loadMs.toFixed(0)} ms`, ""];
	const mismatches = [];
	for (let i = 0; i < m.workloads.length; i++) {
		const a = m.workloads[i], b = t.workloads[i];
		// Turso returns raw rows as arrays, musql's query() too; both normalised to numbers.
		if (a.answers !== b.answers) mismatches.push(a.label);
		const ratio = b.us / a.us;
		lines.push(`${a.label.padEnd(40)} ${fmt(a.us).padStart(10)} ${fmt(b.us).padStart(10)}  ${ratio >= 1 ? ratio.toFixed(1) + "x faster" : (1 / ratio).toFixed(1) + "x SLOWER"}`);
	}
	if (mismatches.length) lines.push("", "ANSWERS DIFFER: " + mismatches.join(", "));
	out.textContent = lines.join("\n");
	window.benchResult = { musql: m, turso: t, mismatches, text: out.textContent };
} catch (e) {
	out.textContent = "error: " + e.message;
	window.benchResult = { error: e.message };
}
