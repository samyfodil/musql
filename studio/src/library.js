// The query history and the saved queries: per browser, in localStorage, so a
// page that cannot store (a private window) still works without them.
const HISTORY = "musql-studio:history";
const SAVED = "musql-studio:saved";
const MAX_HISTORY = 100;

function read(key) {
	try {
		return JSON.parse(localStorage.getItem(key) ?? "[]");
	} catch {
		return [];
	}
}
function write(key, v) {
	try {
		localStorage.setItem(key, JSON.stringify(v));
	} catch {
		// storage full or unavailable: the list just is not kept
	}
	dispatchEvent(new Event("musql-library"));
}

/** The statements run, newest first: {sql, at}. */
export const history = () => read(HISTORY);

/** Records sql as run, moving an earlier run of the same text to the top. */
export function remember(sql) {
	write(HISTORY, [{ sql, at: Date.now() }, ...history().filter((h) => h.sql !== sql)].slice(0, MAX_HISTORY));
}

/** The saved queries, by name: {name, sql}. */
export const saved = () => read(SAVED);

/** Saves sql under name, replacing a query of that name. */
export function save(name, sql) {
	write(SAVED, [...saved().filter((q) => q.name !== name), { name, sql }].sort((a, b) => a.name.localeCompare(b.name)));
}

export function unsave(name) {
	write(SAVED, saved().filter((q) => q.name !== name));
}
