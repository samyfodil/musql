// The page's side of worker.js: every database call is a message, answered
// by id.
const worker = new Worker(new URL("./worker.js", import.meta.url), { type: "module" });
const pending = new Map();
let nextId = 0;

export const ready = new Promise((resolve, reject) => {
	worker.addEventListener("message", function first({ data }) {
		if (data.ready) {
			worker.removeEventListener("message", first);
			resolve();
		}
	});
	worker.addEventListener("error", (e) => reject(new Error(e.message || "the database worker failed to start")));
});

worker.addEventListener("message", ({ data }) => {
	const p = pending.get(data.id);
	if (!p) return;
	pending.delete(data.id);
	if (data.error !== undefined) p.reject(new Error(data.error));
	else p.resolve(data.result);
});

export async function call(op, ...args) {
	await ready;
	const id = ++nextId;
	return new Promise((resolve, reject) => {
		pending.set(id, { resolve, reject });
		worker.postMessage({ id, op, args });
	});
}

export const query = (sql, params) => call("query", sql, params);
export const run = (sql, params) => call("run", sql, params);

/** Rows as objects keyed by column name. */
export async function rows(sql, params) {
	const r = await query(sql, params);
	return r.rows.map((row) => Object.fromEntries(r.columns.map((c, i) => [c, row[i]])));
}

/** A quoted SQL identifier. */
export const ident = (s) => `"${String(s).replace(/"/g, '""')}"`;
