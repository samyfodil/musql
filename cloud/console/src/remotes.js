// Remote connections: a Hrana/libSQL URL and its token, kept in this
// browser's localStorage so the switcher can reopen them. The token is stored
// as given; whoever can read this browser's storage can use it.

const KEY = "musql-console:remotes";

/** The kept connections, newest first: [{name, url, token, at}]. */
export function remotes() {
	try {
		return JSON.parse(localStorage.getItem(KEY) ?? "[]");
	} catch {
		return [];
	}
}

function keep(list) {
	try {
		localStorage.setItem(KEY, JSON.stringify(list));
	} catch {
		// a private window keeps it for this page only
	}
}

/** keepRemote saves c (by name), as the newest. */
export const keepRemote = (c) => keep([{ ...c, at: Date.now() }, ...remotes().filter((r) => r.name !== c.name)]);

export const forgetRemote = (name) => keep(remotes().filter((r) => r.name !== name));

export const remoteNamed = (name) => remotes().find((r) => r.name === name);

/** Whether url is one @libsql/client's web build can reach. */
export const validURL = (url) => /^(https?|libsql|wss?):\/\/[^/\s]+/i.test(url.trim());
