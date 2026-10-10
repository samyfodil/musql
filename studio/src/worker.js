// The database side of musql studio: the npm package in a module worker, so
// the JIT is on (browsers compile wasm synchronously only off the main
// thread). The page sends {id, op, args}; the answer is {id, result} or
// {id, error}.
import { open } from "@samyfodil/musql";

let db = null;
let seq = 0;

const ops = {
	// open(data?) replaces the current database: a fresh one, or the bytes of a
	// musql or C SQLite file (the package tells them apart by the header).
	async open(data) {
		db?.close();
		db = await open(`studio-${++seq}.musq`, data ? { data } : {});
		return true;
	},
	// query runs one statement and returns its rows; a script of several runs
	// through run() instead, which returns its changes.
	query(sql, params = []) {
		const t = performance.now();
		let res;
		try {
			res = db.query(sql, ...params);
		} catch (e) {
			if (!/multi-statement script/.test(e.message)) throw e;
			const { changes } = db.run(sql, ...params);
			res = { columns: [], rows: [], changes, script: true };
		}
		res.ms = performance.now() - t;
		return res;
	},
	run: (sql, params = []) => db.run(sql, ...params),
	exportMusql: () => db.export(),
	exportSQLite: () => db.exportSQLite(),
};

onmessage = async ({ data: { id, op, args = [] } }) => {
	try {
		const result = await ops[op](...args);
		postMessage({ id, result });
	} catch (e) {
		postMessage({ id, error: e.message || String(e) });
	}
};
postMessage({ ready: true });
