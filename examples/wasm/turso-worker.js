// Turso's browser build in a module Web Worker, answering the same messages
// as worker.js does for musql: exec, query (sql, args) -> { columns, rows, ms }.
import { connect } from "./vendor/turso-0.8.2.js";

let db;
const ready = connect(":memory:").then((d) => {
	db = d;
	postMessage({ ready: true, engine: "turso" });
}, (e) => postMessage({ error: String(e) }));

const ops = {
	async exec(sql) {
		await db.exec(sql);
	},
	async query(sql, args = []) {
		const t = performance.now();
		const stmt = db.prepare(sql);
		const rows = (await stmt.all(...args)).map((r) => Object.values(r));
		const ms = performance.now() - t;
		return { columns: stmt.columns ? stmt.columns().map((c) => c.name) : [], rows, ms };
	},
};

onmessage = async ({ data: { id, op, args = [] } }) => {
	await ready;
	try {
		postMessage({ id, result: await ops[op](...args) });
	} catch (e) {
		postMessage({ id, error: String(e?.message ?? e) });
	}
};
