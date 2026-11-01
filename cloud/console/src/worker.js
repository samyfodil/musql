// The database side of the console: the npm package in a module worker, where
// browsers compile wasm synchronously so the JIT is on. The page sends {id, op,
// args} and gets {id, result} or {id, error}.
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
	// split splits a script into statements as the engine does; with a remote
	// database open there may be no local one, so it opens an empty one to ask.
	async split(sql) {
		db ??= await open(`studio-${++seq}.musq`, {});
		return db.split(sql);
	},
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
