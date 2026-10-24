// How the console keeps its sign-in alive, under Node on a fake clock, with a
// stand-in for the server's token endpoint: node auth.test.mjs
import assert from "node:assert/strict";
import { mock, test } from "node:test";

const MIN = 60e3;
const HOUR = 60 * MIN;
const REFRESH = "musql-cloud:refresh";
const settle = () => new Promise((resolve) => setImmediate(resolve));

// ok is a token response: access token A<n>, refresh token R<n>, good for an hour.
const ok = (n) => ({ body: { access_token: `A${n}`, token_type: "Bearer", expires_in: 3600, refresh_token: `R${n}` } });

let copies = 0;

// load runs a fresh copy of auth.js in a console served at https://cloud.test,
// holding the refresh token R0, with handle(params, call number) answering its
// token requests: {status, body}, or an Error for a request that never arrives.
async function load(t, handle, { signedIn = true } = {}) {
	mock.timers.enable({ apis: ["setTimeout", "Date"], now: 0 });
	const store = new Map(signedIn ? [[REFRESH, "R0"]] : []);
	const calls = [];
	const stubs = {
		location: { pathname: "/console/", origin: "https://cloud.test", search: "", hash: "", assign() {} },
		sessionStorage: { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => store.set(k, v), removeItem: (k) => store.delete(k) },
		fetch: async (url, init) => {
			assert.equal(url, "https://cloud.test/oidc/token");
			calls.push(Object.fromEntries(init.body));
			const r = handle(calls.at(-1), calls.length);
			if (r instanceof Error) throw r;
			return new Response(JSON.stringify(r.body ?? {}), { status: r.status ?? 200 });
		},
	};
	const was = Object.fromEntries(Object.keys(stubs).map((k) => [k, Object.getOwnPropertyDescriptor(globalThis, k)]));
	for (const [k, value] of Object.entries(stubs)) Object.defineProperty(globalThis, k, { value, configurable: true, writable: true });
	t.after(() => {
		mock.timers.reset();
		for (const [k, d] of Object.entries(was)) {
			if (d) Object.defineProperty(globalThis, k, d);
			else delete globalThis[k];
		}
	});
	const auth = await import(`./src/cloud/auth.js?copy=${++copies}`);
	return { auth, store, calls };
}

test("the session is refreshed halfway through the access token's life", async (t) => {
	const { auth, store, calls } = await load(t, (_, n) => ok(n));
	await auth.start();
	assert.equal(auth.authState().status, "in");
	assert.equal(await auth.token(), "A1");
	mock.timers.tick(30 * MIN - 1);
	await settle();
	assert.equal(calls.length, 1);
	mock.timers.tick(1);
	await settle();
	assert.equal(calls.length, 2);
	assert.deepEqual(calls[1], { client_id: "console", grant_type: "refresh_token", refresh_token: "R1" }, "the rotated refresh token");
	assert.equal(await auth.token(), "A2");
	assert.equal(store.get(REFRESH), "R2");
	assert.equal(auth.authState().status, "in");
});

test("an access token about to expire is refreshed at its next use", async (t) => {
	const { auth, calls } = await load(t, (_, n) => ok(n));
	await auth.start();
	mock.timers.setTime(HOUR - 10e3); // the page slept through the timer
	assert.equal(await auth.token(), "A2");
	assert.equal(calls.length, 2);
});

test("uses at the same time share one refresh", async (t) => {
	const { auth, calls } = await load(t, (_, n) => ok(n));
	await auth.start();
	mock.timers.setTime(HOUR);
	assert.deepEqual(await Promise.all([auth.token(), auth.token(), auth.token()]), ["A2", "A2", "A2"]);
	assert.equal(calls.length, 2);
});

test("a network failure does not sign out: the refresh token is kept for the next try", async (t) => {
	let down = false;
	let issued = 0;
	const { auth, store } = await load(t, () => (down ? new TypeError("fetch failed") : ok(++issued)));
	await auth.start();
	down = true;
	mock.timers.tick(30 * MIN);
	await settle();
	assert.equal(auth.authState().status, "in", "the timer's refresh failed");
	assert.equal(store.get(REFRESH), "R1");
	mock.timers.setTime(HOUR);
	await assert.rejects(auth.token(), /fetch failed/);
	assert.equal(auth.authState().status, "in");
	assert.equal(store.get(REFRESH), "R1");
	down = false;
	assert.equal(await auth.token(), "A2");
	assert.equal(store.get(REFRESH), "R2");
});

for (const status of [500, 502, 503, 408, 429]) {
	test(`a ${status} from the token endpoint does not sign out`, async (t) => {
		let answer = null;
		const { auth, store } = await load(t, (_, n) => answer ?? ok(n));
		await auth.start();
		answer = { status, body: { error: "temporarily_unavailable" } };
		mock.timers.setTime(HOUR);
		await assert.rejects(auth.token());
		assert.equal(auth.authState().status, "in");
		assert.equal(store.get(REFRESH), "R1");
	});
}

for (const status of [400, 401]) {
	test(`a ${status} refusal of the refresh token signs out, with the reason`, async (t) => {
		let answer = null;
		const { auth, store } = await load(t, (_, n) => answer ?? ok(n));
		await auth.start();
		answer = { status, body: { error: "invalid_grant", error_description: "invalid refresh token" } };
		mock.timers.tick(30 * MIN);
		await settle();
		assert.deepEqual(auth.authState(), { status: "out", error: "Signed out: invalid refresh token" });
		assert.equal(store.get(REFRESH), undefined);
		await assert.rejects(auth.token(), /not signed in/);
	});
}

test("listeners hear the session end", async (t) => {
	let answer = null;
	const { auth } = await load(t, (_, n) => answer ?? ok(n));
	await auth.start();
	const heard = [];
	auth.subscribe((s) => heard.push(s.status));
	answer = { status: 400, body: { error: "invalid_grant" } };
	mock.timers.tick(30 * MIN);
	await settle();
	assert.deepEqual(heard, ["out"]);
});

test("starting with a refresh token that is turned down ends signed out", async (t) => {
	const { auth, store } = await load(t, () => ({ status: 400, body: { error: "invalid_grant", error_description: "invalid refresh token" } }));
	await auth.start();
	assert.deepEqual(auth.authState(), { status: "out", error: "Signed out: invalid refresh token" });
	assert.equal(store.get(REFRESH), undefined);
});

test("starting while the server cannot be reached ends signed out for now, with the refresh token kept", async (t) => {
	const { auth, store } = await load(t, () => new TypeError("fetch failed"));
	await auth.start();
	const s = auth.authState();
	assert.equal(s.status, "out");
	assert.match(s.error, /Could not reach cloud\.test: fetch failed/);
	assert.equal(store.get(REFRESH), "R0");
});

test("starting without a refresh token is signed out", async (t) => {
	const { auth, calls } = await load(t, () => assert.fail("no request expected"), { signedIn: false });
	await auth.start();
	assert.deepEqual(auth.authState(), { status: "out", error: null });
	assert.equal(calls.length, 0);
});
