// The management API client over the console's sign-in, against a stub of the
// server, on a fake clock: node api.test.mjs
// The tests share one signed-in session and run in the order written.
import assert from "node:assert/strict";
import { registerHooks } from "node:module";
import { mock, test } from "node:test";
import { Code, ConnectError } from "@connectrpc/connect";

// The generated client imports "./common_pb" and the like, which a bundler
// resolves and Node does not.
registerHooks({
	resolve(specifier, context, next) {
		try {
			return next(specifier, context);
		} catch (e) {
			if (e.code !== "ERR_MODULE_NOT_FOUND" || !specifier.startsWith(".") || /\.[cm]?js$/.test(specifier)) throw e;
			return next(`${specifier}.js`, context);
		}
	},
});

const HOUR = 3600e3;
const settle = () => new Promise((resolve) => setImmediate(resolve));

mock.timers.enable({ apis: ["setTimeout", "Date"], now: 0 });

// The stub server: it holds the access and refresh tokens it has issued, each
// refresh token good once, and logs every request. With deny set it refuses
// every access token, as for a user who has lost access.
const live = { access: new Set(), refresh: new Set(["R0"]), deny: false };
const log = [];
let issued = 0;
let dbTokens = 0;

function grant() {
	const n = ++issued;
	live.access.add(`A${n}`);
	live.refresh.add(`R${n}`);
	return { access_token: `A${n}`, token_type: "Bearer", expires_in: 3600, refresh_token: `R${n}` };
}

const json = (status, body) => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

const store = new Map([["musql-cloud:refresh", "R0"]]);
const stubs = {
	location: { pathname: "/console/", origin: "https://cloud.test", search: "", hash: "", assign() {} },
	sessionStorage: { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => store.set(k, v), removeItem: (k) => store.delete(k) },
	fetch: async (url, init = {}) => {
		const { pathname } = new URL(url);
		const body = init.body instanceof URLSearchParams ? Object.fromEntries(init.body) : init.body ? JSON.parse(new TextDecoder().decode(init.body)) : null;
		const auth = new Headers(init.headers).get("authorization");
		log.push({ pathname, auth, body });
		if (pathname === "/oidc/token") {
			return live.refresh.delete(body.refresh_token) ? json(200, grant()) : json(400, { error: "invalid_grant", error_description: "invalid refresh token" });
		}
		if (live.deny || !live.access.has(auth?.slice("Bearer ".length))) return json(401, { code: "unauthenticated", message: "sign in first" });
		if (pathname === "/cloud.v1.DatabaseService/CreateToken") return json(200, { token: `D${++dbTokens}` });
		return json(404, { code: "unimplemented", message: pathname });
	},
};
for (const [k, value] of Object.entries(stubs)) Object.defineProperty(globalThis, k, { value, configurable: true, writable: true });

const auth = await import("./src/cloud/auth.js");
const { api, databaseToken } = await import("./src/cloud/api.js");

const refreshes = () => log.filter((r) => r.pathname === "/oidc/token").length;
const creates = () => log.filter((r) => r.pathname === "/cloud.v1.DatabaseService/CreateToken");

test("a console holding a refresh token signs in", async () => {
	await auth.start();
	assert.equal(auth.authState().status, "in");
	assert.equal(refreshes(), 1);
});

test("a database token is asked for an hour, with the access the caller needs", async () => {
	assert.deepEqual(await databaseToken("acme", "main", false), { token: "D1", exp: HOUR });
	assert.deepEqual(await databaseToken("acme", "main", true), { token: "D2", exp: HOUR });
	const [full, readOnly] = creates();
	assert.equal(full.auth, "Bearer A1");
	assert.deepEqual(full.body, { org: "acme", name: "main", access: "ACCESS_FULL", expiresAt: "1970-01-01T01:00:00Z" });
	assert.equal(readOnly.body.access, "ACCESS_READ_ONLY");
});

test("an access token the server turns down is refreshed, and the call sent again, once", async () => {
	live.access.delete("A1");
	log.length = 0;
	assert.equal((await databaseToken("acme", "main", false)).token, "D3");
	assert.deepEqual(log.map((r) => [r.pathname, r.auth]), [
		["/cloud.v1.DatabaseService/CreateToken", "Bearer A1"],
		["/oidc/token", null],
		["/cloud.v1.DatabaseService/CreateToken", "Bearer A2"],
	]);
	assert.equal(auth.authState().status, "in");
});

test("a call refused again after the refresh fails with the server's answer", async () => {
	live.deny = true;
	log.length = 0;
	await assert.rejects(api.db.createToken({ org: "acme", name: "main" }), (e) => ConnectError.from(e).code === Code.Unauthenticated);
	assert.equal(creates().length, 2, "one retry");
	assert.equal(refreshes(), 1, "one refresh");
	assert.equal(auth.authState().status, "in", "the session itself is fine");
	live.deny = false;
});

test("the session refreshes itself halfway through the access token's life", async () => {
	log.length = 0;
	mock.timers.tick(HOUR / 2);
	await settle();
	assert.equal(refreshes(), 1);
	assert.equal(auth.authState().status, "in");
});

test("when the refresh token is revoked, the session ends and database tokens say why", async () => {
	live.access.clear();
	live.refresh.clear();
	log.length = 0;
	await assert.rejects(databaseToken("acme", "main", false), /^Error: Signed out: sign in again to reach acme\/main\.$/);
	assert.deepEqual(auth.authState(), { status: "out", error: "Signed out: invalid refresh token" });
	assert.equal(store.has("musql-cloud:refresh"), false);
	await assert.rejects(databaseToken("acme", "main", false), /Signed out/, "and stays that way, without asking the server again");
	assert.equal(refreshes(), 1);
});
