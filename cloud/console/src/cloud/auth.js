// Signing in to a musql cloud server: OIDC authorization code with PKCE (S256)
// as its public "console" client. The access token lives in memory and the
// refresh token in sessionStorage, so a reload stays signed in and a new tab
// signs in again. Only the console the server serves can complete a sign-in.

const REFRESH = "musql-cloud:refresh";
const PENDING = "musql-cloud:pending";
const CLIENT = "console";
const SCOPE = "openid profile email offline_access";

const underConsole = location.pathname.startsWith("/console/") || location.pathname === "/console";

/** The server this console belongs to: its own origin when served under /console/, else none, until configureAuth names one. */
export let server = underConsole ? location.origin : null;

// Where the server sends the browser back with a code, and where it lands
// after; both on this page's origin.
let callbackPath = "/console/callback";
let home = "/console/";
// The app's own sign-in, when it has one: {getToken, signIn, signOut}.
let external = null;

const redirectURI = () => `${location.origin}${callbackPath}`;

let access = null; // {token, exp}
let refreshing = null;
let timer = 0;
const listeners = new Set();
let state = { status: server ? "starting" : "none", error: null };

/**
 * configureAuth binds sign-in to one server (an origin, or null for none).
 * With getToken the app signs in itself and every call carries its token;
 * otherwise the console signs in to server with OIDC, coming back at
 * callbackPath (default /console/callback under /console/, else /callback).
 * Call it before start.
 */
export function configureAuth(o) {
	server = o.server ?? null;
	callbackPath = o.callbackPath ?? (underConsole ? "/console/callback" : "/callback");
	home = o.home ?? (underConsole ? "/console/" : "/");
	external = o.getToken ? { getToken: o.getToken, signIn: o.signIn, signOut: o.signOut } : null;
	state = { status: external ? "in" : server ? "starting" : "none", error: null };
}

function set(next) {
	state = { ...state, ...next };
	listeners.forEach((f) => f(state));
}

/** subscribe(f) calls f(state) on every change: {status: "none"|"starting"|"out"|"in", error}. */
export function subscribe(f) {
	listeners.add(f);
	return () => listeners.delete(f);
}
export const authState = () => state;

function storage(f) {
	try {
		return f(sessionStorage);
	} catch {
		return null;
	}
}

const b64url = (bytes) => btoa(String.fromCharCode(...bytes)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
const random = (n) => b64url(crypto.getRandomValues(new Uint8Array(n)));

/** signIn sends the browser to the server's authorize endpoint; it comes back at the callback path. */
export async function signIn() {
	if (external) return external.signIn?.();
	const verifier = random(48);
	const challenge = b64url(new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier))));
	const st = random(16);
	storage((s) => s.setItem(PENDING, JSON.stringify({ verifier, state: st, hash: location.hash })));
	const q = new URLSearchParams({
		client_id: CLIENT,
		redirect_uri: redirectURI(),
		response_type: "code",
		scope: SCOPE,
		state: st,
		code_challenge: challenge,
		code_challenge_method: "S256",
	});
	location.assign(`${server}/oidc/authorize?${q}`);
}

async function tokenRequest(params) {
	const res = await fetch(`${server}/oidc/token`, {
		method: "POST",
		headers: { "Content-Type": "application/x-www-form-urlencoded" },
		body: new URLSearchParams({ client_id: CLIENT, ...params }),
	});
	const body = await res.json().catch(() => ({}));
	if (!res.ok) {
		const err = new Error(body.error_description || body.error || `token endpoint: HTTP ${res.status}`);
		// A 4xx is the server turning the grant down; a server error or a busy one says nothing about it.
		err.refused = res.status >= 400 && res.status < 500 && res.status !== 408 && res.status !== 429;
		throw err;
	}
	return body;
}

function took(body) {
	access = { token: body.access_token, exp: Date.now() + body.expires_in * 1000 };
	if (body.refresh_token) storage((s) => s.setItem(REFRESH, body.refresh_token));
	clearTimeout(timer);
	// Refresh a minute before expiry, or halfway through a short lifetime.
	timer = setTimeout(() => refresh().catch(() => {}), Math.max(5000, Math.min(body.expires_in * 500, body.expires_in * 1000 - 60000)));
	set({ status: "in", error: null });
}

/** refresh trades the refresh token for new tokens; concurrent callers share one request, since a refresh token is single-use. */
export function refresh() {
	if (external) return Promise.resolve();
	if (refreshing) return refreshing;
	const rt = storage((s) => s.getItem(REFRESH));
	if (!rt) {
		set({ status: "out" });
		return Promise.reject(new Error("not signed in"));
	}
	refreshing = tokenRequest({ grant_type: "refresh_token", refresh_token: rt })
		.then(took, (e) => {
			// Only the server refusing the refresh token ends the session. A network failure leaves it for the next try.
			if (e.refused) {
				access = null;
				storage((s) => s.removeItem(REFRESH));
				set({ status: "out", error: `Signed out: ${e.message}` });
			}
			throw e;
		})
		.finally(() => (refreshing = null));
	return refreshing;
}

/** token resolves to a live access token, refreshing first when it is about to expire. */
export async function token() {
	if (external) return external.getToken();
	if (access && access.exp - Date.now() > 30000) return access.token;
	await refresh();
	return access.token;
}

/** signOut forgets the tokens and ends the server's sign-in session. */
export async function signOut() {
	if (external) return external.signOut?.();
	const rt = storage((s) => s.getItem(REFRESH));
	access = null;
	clearTimeout(timer);
	storage((s) => s.removeItem(REFRESH));
	set({ status: "out" });
	// Revoke the refresh token server-side, but never let a failure keep the user signed in.
	if (rt) {
		await fetch(`${server}/oidc/revoke`, {
			method: "POST",
			headers: { "Content-Type": "application/x-www-form-urlencoded" },
			body: new URLSearchParams({ client_id: CLIENT, token: rt, token_type_hint: "refresh_token" }),
		}).catch(() => {});
	}
	location.assign(`${server}/oidc/logout?${new URLSearchParams({ post_logout_redirect_uri: `${location.origin}${home}` })}`);
}

/** start finishes a sign-in at the callback path, or resumes from a kept refresh token. */
export async function start() {
	if (!server || external) return;
	if (location.pathname === callbackPath) {
		const q = new URLSearchParams(location.search);
		const pending = storage((s) => JSON.parse(s.getItem(PENDING) ?? "null"));
		storage((s) => s.removeItem(PENDING));
		history.replaceState(null, "", `${home}${pending?.hash ?? ""}`);
		if (q.get("error")) return set({ status: "out", error: `Sign-in failed: ${q.get("error_description") || q.get("error")}` });
		if (!pending || q.get("state") !== pending.state) return set({ status: "out", error: "Sign-in failed: the response does not match this tab's request." });
		try {
			took(await tokenRequest({ grant_type: "authorization_code", code: q.get("code"), redirect_uri: redirectURI(), code_verifier: pending.verifier }));
		} catch (e) {
			set({ status: "out", error: `Sign-in failed: ${e.message}` });
		}
		return;
	}
	if (storage((s) => s.getItem(REFRESH))) {
		await refresh().catch((e) => {
			if (!e.refused) set({ status: "out", error: `Could not reach ${new URL(server).host}: ${e.message}` });
		});
	} else set({ status: "out" });
}
