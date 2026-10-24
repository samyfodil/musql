// A remote database's token that renews itself, on a fake clock and with the
// real @libsql/client against a stub Hrana server, under Node:
// node renew.test.mjs
import assert from "node:assert/strict";
import http from "node:http";
import { mock, test } from "node:test";
import { createClient } from "@libsql/client/web";
import { authFetch, tokenSession } from "./src/renew.js";

const MIN = 60e3;
const HOUR = 60 * MIN;

// A test runs with setTimeout and Date on the fake clock, which starts at 0.
function clock(t) {
	mock.timers.enable({ apis: ["setTimeout", "Date"], now: 0 });
	t.after(() => mock.timers.reset());
}

// Lets the promise chains started by a timer finish; setImmediate is not faked.
const settle = () => new Promise((resolve) => setImmediate(resolve));

// minter hands out T1, T2, ..., each good for an hour from when it is asked for.
function minter() {
	const m = {
		calls: 0,
		fail: null,
		async mint() {
			m.calls++;
			if (m.fail) throw m.fail;
			return { token: `T${m.calls}`, exp: Date.now() + HOUR };
		},
	};
	return m;
}

// A server that accepts only the tokens in valid, and records what it was sent.
function server(valid) {
	const seen = [];
	return {
		seen,
		async send(req) {
			const auth = req.headers.get("authorization");
			seen.push({ auth, method: req.method, body: await req.text() });
			return valid.has(auth) ? new Response("ok") : new Response('{"message":"unauthorized"}', { status: 401 });
		},
	};
}

const post = (body = '{"requests":[]}') => new Request("https://cloud.test/db/acme/main/v2/pipeline", { method: "POST", headers: { "content-type": "application/json" }, body });

test("a token is renewed halfway through its life, before it expires", async (t) => {
	clock(t);
	const m = minter();
	const s = tokenSession({ token: "T0", exp: HOUR, mint: m.mint });
	mock.timers.tick(30 * MIN - 1);
	await settle();
	assert.equal(m.calls, 0);
	assert.equal(await s.token(), "T0");
	mock.timers.tick(1);
	await settle();
	assert.equal(m.calls, 1);
	assert.equal(await s.token(), "T1");
	assert.equal(m.calls, 1, "using the renewed token mints nothing");
	// T1 was minted at 30 minutes and lasts to 90: its half is at 60.
	mock.timers.tick(30 * MIN - 1);
	await settle();
	assert.equal(m.calls, 1);
	mock.timers.tick(1);
	await settle();
	assert.equal(m.calls, 2);
	assert.equal(await s.token(), "T2");
	s.stop();
});

test("a page that slept through the timer renews at the next use", async (t) => {
	clock(t);
	const m = minter();
	const s = tokenSession({ token: "T0", exp: HOUR, mint: m.mint });
	mock.timers.setTime(40 * MIN); // the clock moved, no timer ran
	assert.equal(await s.token(), "T1");
	assert.equal(await s.token(), "T1");
	assert.equal(m.calls, 1);
	mock.timers.tick(HOUR); // the timer the sleep skipped was replaced, not left to fire
	await settle();
	assert.equal(m.calls, 2, "T1, minted at 40 minutes, is renewed at 70");
	s.stop();
});

test("uses at the same time share one renewal", async (t) => {
	clock(t);
	const m = minter();
	const s = tokenSession({ token: "T0", exp: HOUR, mint: m.mint });
	mock.timers.setTime(40 * MIN);
	assert.deepEqual(await Promise.all([s.token(), s.token(), s.token()]), ["T1", "T1", "T1"]);
	assert.equal(m.calls, 1);
	s.stop();
});

test("a failed renewal keeps the token going and is tried again halfway to its expiry", async (t) => {
	clock(t);
	const m = minter();
	m.fail = new Error("offline");
	const s = tokenSession({ token: "T0", exp: HOUR, mint: m.mint });
	mock.timers.tick(30 * MIN);
	await settle();
	assert.equal(m.calls, 1);
	assert.equal(await s.token(), "T0", "still good for 30 minutes");
	assert.equal(m.calls, 1, "no new attempt before the retry");
	m.fail = null;
	mock.timers.tick(15 * MIN - 1);
	await settle();
	assert.equal(m.calls, 1);
	mock.timers.tick(1);
	await settle();
	assert.equal(m.calls, 2);
	assert.equal(await s.token(), "T2");
	s.stop();
});

test("retries stop short of the expiry and are not endless", async (t) => {
	clock(t);
	const m = minter();
	m.fail = new Error("offline");
	const s = tokenSession({ token: "T0", exp: HOUR, mint: m.mint });
	const run = async (minutes) => {
		for (let i = 0; i < minutes; i++) {
			mock.timers.tick(MIN);
			await settle();
		}
	};
	await run(61);
	const tried = m.calls;
	assert.ok(tried >= 5 && tried <= 10, `${tried} attempts between the half and the expiry`);
	await run(120);
	assert.equal(m.calls, tried, "none after the expiry");
	s.stop();
});

test("a token past its expiry that cannot be renewed fails the use", async (t) => {
	clock(t);
	const m = minter();
	m.fail = new Error("Signed out: sign in again");
	const s = tokenSession({ token: "T0", exp: HOUR, mint: m.mint });
	mock.timers.setTime(HOUR + 1);
	await assert.rejects(s.token(), /Signed out/);
	m.fail = null;
	assert.equal(await s.token(), "T2", "and works again once it can be renewed");
	s.stop();
});

test("a token without an expiry is renewed only when refused", async (t) => {
	clock(t);
	const m = minter();
	const s = tokenSession({ token: "T0", mint: m.mint });
	mock.timers.tick(100 * HOUR);
	await settle();
	assert.equal(await s.token(), "T0");
	assert.equal(m.calls, 0);
	assert.equal(await s.renew("T0"), "T1");
	s.stop();
});

test("stop ends the renewals", async (t) => {
	clock(t);
	const m = minter();
	const s = tokenSession({ token: "T0", exp: HOUR, mint: m.mint });
	s.stop();
	mock.timers.tick(2 * HOUR);
	await settle();
	assert.equal(m.calls, 0);
});

test("a token another call has already replaced is not renewed again", async (t) => {
	clock(t);
	const m = minter();
	const s = tokenSession({ token: "T0", exp: HOUR, mint: m.mint });
	assert.equal(await s.renew("T0"), "T1");
	assert.equal(await s.renew("T0"), "T1", "a late refusal of the old token");
	assert.equal(m.calls, 1);
	assert.equal(await s.renew("T1"), "T2", "a refusal of the current one mints");
	s.stop();
});

test("a request refused with 401 is sent again, once, with a renewed token", async (t) => {
	clock(t);
	const m = minter();
	const s = tokenSession({ token: "T0", exp: HOUR, mint: m.mint });
	const srv = server(new Set(["Bearer T1"]));
	const res = await authFetch(s, srv.send)(post('{"q":1}'));
	assert.equal(res.status, 200);
	assert.deepEqual(srv.seen, [
		{ auth: "Bearer T0", method: "POST", body: '{"q":1}' },
		{ auth: "Bearer T1", method: "POST", body: '{"q":1}' },
	]);
	assert.equal(m.calls, 1);
	s.stop();
});

test("a second 401 is the answer: one renewal, one retry", async (t) => {
	clock(t);
	const m = minter();
	const s = tokenSession({ token: "T0", exp: HOUR, mint: m.mint });
	const srv = server(new Set());
	const res = await authFetch(s, srv.send)(post());
	assert.equal(res.status, 401);
	assert.equal(srv.seen.length, 2);
	assert.equal(m.calls, 1);
	s.stop();
});

test("requests refused together share one renewal", async (t) => {
	clock(t);
	const m = minter();
	const s = tokenSession({ token: "T0", exp: HOUR, mint: m.mint });
	const srv = server(new Set(["Bearer T1"]));
	const f = authFetch(s, srv.send);
	const all = await Promise.all([f(post("1")), f(post("2")), f(post("3"))]);
	assert.deepEqual(all.map((r) => r.status), [200, 200, 200]);
	assert.equal(m.calls, 1);
	assert.deepEqual(srv.seen.filter((x) => x.auth === "Bearer T1").map((x) => x.body).sort(), ["1", "2", "3"]);
	s.stop();
});

test("a stale token is renewed before the request goes out", async (t) => {
	clock(t);
	const m = minter();
	const s = tokenSession({ token: "T0", exp: HOUR, mint: m.mint });
	mock.timers.setTime(45 * MIN);
	const srv = server(new Set(["Bearer T1"]));
	const res = await authFetch(s, srv.send)(post());
	assert.equal(res.status, 200);
	assert.deepEqual(srv.seen.map((x) => x.auth), ["Bearer T1"], "never sent with the old one");
	s.stop();
});

test("a renewal that fails fails the request with its reason", async (t) => {
	clock(t);
	const m = minter();
	m.fail = new Error("Signed out: sign in again to reach acme/main.");
	const s = tokenSession({ token: "T0", exp: HOUR, mint: m.mint });
	const srv = server(new Set(["Bearer T1"]));
	await assert.rejects(authFetch(s, srv.send)(post()), /sign in again to reach acme\/main/);
	assert.equal(srv.seen.length, 1);
	s.stop();
});

test("requests without a body pass through", async (t) => {
	clock(t);
	const m = minter();
	const s = tokenSession({ token: "T0", exp: HOUR, mint: m.mint });
	const srv = server(new Set(["Bearer T0"]));
	const res = await authFetch(s, srv.send)(new Request("https://cloud.test/db/acme/main/health"));
	assert.equal(res.status, 200);
	assert.deepEqual(srv.seen, [{ auth: "Bearer T0", method: "GET", body: "" }]);
	s.stop();
});

test("the headers of the request survive the signing", async (t) => {
	clock(t);
	const m = minter();
	const s = tokenSession({ token: "T0", exp: HOUR, mint: m.mint });
	let sent;
	const f = authFetch(s, async (req) => {
		sent = req;
		return new Response("ok");
	});
	await f(new Request("https://cloud.test/x", { method: "POST", headers: { "content-type": "application/x-protobuf", authorization: "Bearer stale" }, body: "b" }));
	assert.equal(sent.headers.get("content-type"), "application/x-protobuf");
	assert.equal(sent.headers.get("authorization"), "Bearer T0");
	assert.equal(sent.url, "https://cloud.test/x");
	s.stop();
});

// ---- the real client, real timers, tokens that live a fraction of a second ----

// hrana is a stub Hrana-over-HTTP server that answers SELECT 1 to a bearer
// token in valid (token -> expiry, ms) and refuses the rest with 401. log
// records every request's token and whether it was live.
async function hrana(t) {
	const valid = new Map();
	const log = [];
	const srv = http.createServer(async (req, res) => {
		let body = "";
		for await (const chunk of req) body += chunk;
		const token = /^Bearer (.*)$/.exec(req.headers.authorization ?? "")?.[1];
		const live = valid.get(token) > Date.now();
		log.push({ token, live });
		res.setHeader("content-type", "application/json");
		if (!live) {
			res.statusCode = 401;
			res.end('{"message":"unauthorized"}');
			return;
		}
		const results = JSON.parse(body).requests.map((r) => ({
			type: "ok",
			response: r.type === "execute"
				? { type: "execute", result: { cols: [{ name: "1", decltype: null }], rows: [[{ type: "integer", value: "1" }]], affected_row_count: 0, last_insert_rowid: null } }
				: { type: r.type },
		}));
		res.end(JSON.stringify({ baton: null, base_url: null, results }));
	});
	await new Promise((resolve) => srv.listen(0, "127.0.0.1", resolve));
	t.after(() => {
		srv.closeAllConnections();
		srv.close();
	});
	return { url: `http://127.0.0.1:${srv.address().port}/db/acme/main/`, valid, log };
}

// issuer mints T0, T1, ... that the server holds as valid for life ms.
function issuer(h, life) {
	const i = {
		n: 0,
		fail: null,
		issue() {
			const token = `T${i.n++}`;
			const exp = Date.now() + life;
			h.valid.set(token, exp);
			return { token, exp };
		},
		async mint() {
			if (i.fail) throw i.fail;
			return i.issue();
		},
	};
	return i;
}

function connection(t, h, i) {
	const first = i.issue();
	const session = tokenSession({ token: first.token, exp: first.exp, mint: i.mint });
	const client = createClient({ url: h.url, authToken: first.token, intMode: "bigint", fetch: authFetch(session) });
	t.after(() => {
		session.stop();
		client.close();
	});
	return { first, client };
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

test("a connection outlives its token: queries keep working across many expiries", async (t) => {
	const h = await hrana(t);
	const i = issuer(h, 300);
	const { first, client } = connection(t, h, i);
	const plain = createClient({ url: h.url, authToken: first.token, intMode: "bigint" });
	t.after(() => plain.close());

	const until = Date.now() + 1500;
	let queries = 0;
	while (Date.now() < until) {
		assert.equal((await client.execute("SELECT 1")).rows[0][0], 1n);
		queries++;
		await sleep(20);
	}
	assert.ok(queries > 20, `${queries} queries`);
	assert.ok(i.n >= 4, `${i.n} tokens for 1.5s of 300ms tokens`);
	assert.ok(h.valid.get(first.token) < Date.now(), "the first token is long expired");
	await assert.rejects(plain.execute("SELECT 1"), /unauthorized|401/, "and a client that cannot renew has died");
});

test("a token revoked while unexpired is renewed when the server refuses it, once", async (t) => {
	const h = await hrana(t);
	const i = issuer(h, HOUR);
	const { client } = connection(t, h, i);
	await client.execute("SELECT 1");
	h.valid.delete("T0");
	h.log.length = 0;
	assert.equal((await client.execute("SELECT 1")).rows[0][0], 1n);
	assert.deepEqual(h.log, [{ token: "T0", live: false }, { token: "T1", live: true }]);
	await client.execute("SELECT 1");
	assert.deepEqual(h.log.at(-1), { token: "T1", live: true });
	assert.equal(i.n, 2);
});

test("signed out: the query fails with the reason, and works again once it can renew", async (t) => {
	const h = await hrana(t);
	const i = issuer(h, HOUR);
	const { client } = connection(t, h, i);
	h.valid.delete("T0");
	i.fail = new Error("Signed out: sign in again to reach acme/main.");
	await assert.rejects(client.execute("SELECT 1"), /sign in again to reach acme\/main/);
	i.fail = null;
	assert.equal((await client.execute("SELECT 1")).rows[0][0], 1n);
});
