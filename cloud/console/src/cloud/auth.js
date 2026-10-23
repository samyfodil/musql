// Signing in to a musql cloud server: OIDC authorization code with PKCE (S256)
// against the server's own provider, as its public "console" client. The
// access token lives in memory only; the refresh token in sessionStorage, so a
// reload in the same tab stays signed in and a new tab signs in again. The
// server registers one redirect URI for the console, <server>/console/callback,
// so signing in only completes in the console the server itself serves.

const REFRESH = "musql-cloud:refresh";
const PENDING = "musql-cloud:pending";
const CLIENT = "console";
const SCOPE = "openid profile email offline_access";

/** The server this console belongs to: its own origin when served under /console/, else none. */
export const server = location.pathname.startsWith("/console/") || location.pathname === "/console" ? location.origin : null;

const redirectURI = () => `${server}/console/callback`;

let access = null; // {token, exp}
let refreshing = null;
let timer = 0;
const listeners = new Set();
let state = { status: server ? "starting" : "none", error: null };

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

/** signIn sends the browser to the server's authorize endpoint; it comes back at /console/callback. */
export async function signIn() {
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
	if (!res.ok) throw new Error(body.error_description || body.error || `token endpoint: HTTP ${res.status}`);
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
	if (refreshing) return refreshing;
	const rt = storage((s) => s.getItem(REFRESH));
	if (!rt) {
		set({ status: "out" });
		return Promise.reject(new Error("not signed in"));
	}
	refreshing = tokenRequest({ grant_type: "refresh_token", refresh_token: rt })
		.then(took, (e) => {
			access = null;
			storage((s) => s.removeItem(REFRESH));
			set({ status: "out", error: `Signed out: ${e.message}` });
			throw e;
		})
		.finally(() => (refreshing = null));
	return refreshing;
}

/** token resolves to a live access token, refreshing first when it is about to expire. */
export async function token() {
	if (access && access.exp - Date.now() > 30000) return access.token;
	await refresh();
	return access.token;
}

/** signOut forgets the tokens and ends the server's sign-in session. */
export async function signOut() {
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
	location.assign(`${server}/oidc/logout?${new URLSearchParams({ post_logout_redirect_uri: `${server}/console/` })}`);
}

/** start finishes a sign-in at /console/callback, or resumes from a kept refresh token. */
export async function start() {
	if (!server) return;
	if (location.pathname === "/console/callback") {
		const q = new URLSearchParams(location.search);
		const pending = storage((s) => JSON.parse(s.getItem(PENDING) ?? "null"));
		storage((s) => s.removeItem(PENDING));
		history.replaceState(null, "", `/console/${pending?.hash ?? ""}`);
		if (q.get("error")) return set({ status: "out", error: `Sign-in failed: ${q.get("error_description") || q.get("error")}` });
		if (!pending || q.get("state") !== pending.state) return set({ status: "out", error: "Sign-in failed: the response does not match this tab's request." });
		try {
			took(await tokenRequest({ grant_type: "authorization_code", code: q.get("code"), redirect_uri: redirectURI(), code_verifier: pending.verifier }));
		} catch (e) {
			set({ status: "out", error: `Sign-in failed: ${e.message}` });
		}
		return;
	}
	if (storage((s) => s.getItem(REFRESH))) await refresh().catch(() => {});
	else set({ status: "out" });
}
