// The page's side of the database: a local one is worker.js, a remote one a
// Hrana/libSQL server through @libsql/client. split always goes to the worker,
// which splits a script as the engine does.
import { authFetch, tokenSession } from "./renew.js";
import { verb } from "./sqltext.js";

const worker = new Worker(new URL("./worker.js", import.meta.url), { type: "module" });
const pending = new Map();
let nextId = 0;
let remoteCalls = 0;

export const ready = new Promise((resolve, reject) => {
	worker.addEventListener("message", function first({ data }) {
		if (data.ready) {
			worker.removeEventListener("message", first);
			resolve();
		}
	});
	worker.addEventListener("error", (e) => reject(new Error(e.message || "the database worker failed to start")));
});

// Listeners told how many calls are in flight, for the page's busy bar.
const busyListeners = new Set();
const busy = () => busyListeners.forEach((f) => f(pending.size + remoteCalls));

/** onBusy(f) calls f(n) whenever the number of calls in flight changes; it returns the unsubscribe. */
export function onBusy(f) {
	busyListeners.add(f);
	return () => busyListeners.delete(f);
}

worker.addEventListener("message", ({ data }) => {
	const p = pending.get(data.id);
	if (!p) return;
	pending.delete(data.id);
	busy();
	if (data.error !== undefined) p.reject(new Error(data.error));
	else p.resolve(data.result);
});

/** call sends op to the local worker. */
export async function call(op, ...args) {
	await ready;
	const id = ++nextId;
	return new Promise((resolve, reject) => {
		pending.set(id, { resolve, reject });
		busy();
		worker.postMessage({ id, op, args });
	});
}

// ---- the remote backend ----

let remote = null; // {client, tx, url, host, session}

async function tracked(f) {
	remoteCalls++;
	busy();
	try {
		return await f();
	} finally {
		remoteCalls--;
		busy();
	}
}

/** The host of a remote URL, for labels. */
export const hostOf = (url) => {
	try {
		return new URL(url.replace(/^libsql:/, "https:")).host;
	} catch {
		return url;
	}
};

/** What is open: {kind: "local"} or {kind: "remote", url, host}. */
export const backend = () => (remote ? { kind: "remote", url: remote.url, host: remote.host } : { kind: "local" });
export const isRemote = () => !!remote;

/**
 * connect opens a remote database for every call to go to. SELECT 1 runs first,
 * so a wrong URL or token fails here and leaves the open database as it was.
 * renewal ({exp, mint}) renews an expiring token (renew.js).
 */
export async function connect(url, authToken, renewal) {
	const { createClient } = await import("@libsql/client/web");
	const session = renewal && tokenSession({ token: authToken, exp: renewal.exp, mint: renewal.mint });
	const client = createClient({ url, authToken: authToken || undefined, intMode: "bigint", fetch: session && authFetch(session) });
	try {
		await tracked(() => client.execute("SELECT 1"));
	} catch (e) {
		session?.stop();
		throw e;
	}
	disconnect();
	remote = { client, tx: null, url, host: hostOf(url), session };
}

function disconnect() {
	if (!remote) return;
	remote.session?.stop();
	remote.tx?.close();
	remote.client.close();
	remote = null;
}

// value turns a remote value into the local worker's: integers as numbers
// when exact, else BigInt; blobs as Uint8Array.
function value(v) {
	if (typeof v === "bigint") return v >= BigInt(Number.MIN_SAFE_INTEGER) && v <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(v) : v;
	if (v instanceof ArrayBuffer) return new Uint8Array(v);
	return v;
}

// remoteExec runs one statement, or a script. Over HTTP each request is its
// own connection, so BEGIN opens a client transaction (a Hrana stream kept
// open) that later statements run on, until COMMIT or ROLLBACK closes it.
async function remoteExec(sql, params = []) {
	const r = remote;
	const t = performance.now();
	const v = verb(sql);
	return tracked(async () => {
		if (v === "BEGIN" && !r.tx) {
			r.tx = await r.client.transaction(/\bDEFERRED\b/i.test(sql) ? "deferred" : "write");
			return { columns: [], rows: [], changes: 0, ms: performance.now() - t };
		}
		if (r.tx && (v === "COMMIT" || v === "END" || (v === "ROLLBACK" && !/^\s*ROLLBACK\s+(TRANSACTION\s+)?TO\b/i.test(sql)))) {
			const tx = r.tx;
			r.tx = null;
			await (v === "ROLLBACK" ? tx.rollback() : tx.commit());
			tx.close();
			return { columns: [], rows: [], changes: 0, ms: performance.now() - t };
		}
		const on = r.tx ?? r.client;
		if (!params.length) {
			const stmts = await call("split", sql);
			if (stmts.length > 1) {
				if (r.tx) for (const s of stmts) await r.tx.execute(s);
				else await r.client.executeMultiple(sql);
				return { columns: [], rows: [], changes: 0, script: true, ms: performance.now() - t };
			}
		}
		const rs = await on.execute({ sql, args: params });
		const n = rs.columns.length;
		return {
			columns: rs.columns,
			types: rs.columnTypes,
			rows: rs.rows.map((row) => Array.from({ length: n }, (_, i) => value(row[i]))),
			changes: rs.rowsAffected,
			lastInsertRowid: rs.lastInsertRowid,
			ms: performance.now() - t,
		};
	});
}

// ---- what the page calls ----

/** open makes a local database the open one: a fresh one, or a file's bytes. */
export async function open(data) {
	disconnect();
	return call("open", data);
}

export const query = (sql, params) => (remote ? remoteExec(sql, params) : call("query", sql, params));

// beforeWrite, when set, runs before every write: the page snapshots the
// database there, for undo, and saves it after.
let beforeWrite = null;
export const onBeforeWrite = (f) => { beforeWrite = f; };

export async function run(sql, params) {
	await beforeWrite?.(sql);
	return remote ? remoteExec(sql, params) : call("run", sql, params);
}

/** The statements of a script, as the engine splits them. */
export const split = (sql) => call("split", sql);

const localOnly = (what) => Promise.reject(new Error(`${what} works on local databases; download a remote one as a SQL dump.`));
export const exportMusql = () => (remote ? localOnly("A .musq export") : call("exportMusql"));
export const exportSQLite = () => (remote ? localOnly("A .sqlite export") : call("exportSQLite"));

/** Rows as objects keyed by column name. */
export async function rows(sql, params) {
	const r = await query(sql, params);
	return r.rows.map((row) => Object.fromEntries(r.columns.map((c, i) => [c, row[i]])));
}

/** A quoted SQL identifier. */
export const ident = (s) => `"${String(s).replace(/"/g, '""')}"`;
