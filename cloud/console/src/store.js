// The databases this browser keeps: each one's bytes in IndexedDB, saved after
// every change, so a reload reopens where the page left off. A page that cannot
// store (a private window, storage blocked) still works; it just forgets.

const DB = "musql-studio";
const STORE = "databases";

function idb() {
	return new Promise((resolve, reject) => {
		const req = indexedDB.open(DB, 1);
		req.onupgradeneeded = () => req.result.createObjectStore(STORE, { keyPath: "name" });
		req.onsuccess = () => resolve(req.result);
		req.onerror = () => reject(req.error);
	});
}

async function tx(mode, f) {
	const db = await idb();
	return new Promise((resolve, reject) => {
		const t = db.transaction(STORE, mode);
		const out = f(t.objectStore(STORE));
		t.oncomplete = () => resolve(out?.result);
		t.onerror = () => reject(t.error);
	});
}

/** Keeps bytes as the database name, with the time. */
export const saveDb = (name, bytes) => tx("readwrite", (s) => s.put({ name, bytes, at: Date.now(), size: bytes.length })).catch(() => {});

/** The bytes kept for name, or undefined. */
export async function loadDb(name) {
	const r = await tx("readonly", (s) => s.get(name)).catch(() => undefined);
	return r?.bytes;
}

/** The kept databases, newest first: [{name, at, size}]. */
export async function listDbs() {
	const all = await tx("readonly", (s) => s.getAll()).catch(() => []);
	return (all ?? []).map(({ name, at, size }) => ({ name, at, size })).sort((a, b) => b.at - a.at);
}

export const forgetDb = (name) => tx("readwrite", (s) => s.delete(name)).catch(() => {});

/** Per-database view state (open tabs), in localStorage. */
export function viewState(name, v) {
	const key = `musql-studio:view:${name}`;
	try {
		if (v === undefined) return JSON.parse(localStorage.getItem(key) ?? "null");
		localStorage.setItem(key, JSON.stringify(v));
	} catch {
		return null;
	}
}
