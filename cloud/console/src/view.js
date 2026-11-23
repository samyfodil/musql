// Per-database view state (open tabs, the section), in localStorage, for
// local and cloud databases alike.

/** viewState(name) reads the view kept for name; viewState(name, v) keeps v. */
export function viewState(name, v) {
	const key = `musql-studio:view:${name}`;
	try {
		if (v === undefined) return JSON.parse(localStorage.getItem(key) ?? "null");
		localStorage.setItem(key, JSON.stringify(v));
	} catch {
		return null;
	}
}
