// A remote database's token that renews itself, so a connection outlives it.
// tokenSession tracks one token and mints the next; authFetch is the fetch
// @libsql/client sends its requests through, signing each with the token of
// the moment.

/**
 * tokenSession holds a token and its expiry (ms since the epoch). mint()
 * resolves to the next {token, exp}. A token is renewed once half its
 * lifetime has passed: by a timer while the page is awake, at the next use if
 * the page slept through it, and whenever the server refuses it. A renewal
 * that fails is tried again halfway to the expiry; the token goes on serving
 * until then, and past it a request that needs a renewal fails with its error.
 */
export function tokenSession({ token, exp, mint }) {
	let cur = { token, exp, at: Date.now() };
	let minting = null;
	let retryAt = 0;
	let timer = 0;
	let stopped = false;

	const halfway = () => cur.at + (cur.exp - cur.at) / 2;

	function arm(at = halfway()) {
		clearTimeout(timer);
		if (!stopped && Number.isFinite(at)) timer = setTimeout(() => renew().catch(() => {}), Math.max(0, at - Date.now()));
	}

	async function mintNext() {
		try {
			const next = await mint();
			cur = { token: next.token, exp: next.exp, at: Date.now() };
			retryAt = 0;
			arm();
			return cur.token;
		} catch (e) {
			const left = cur.exp - Date.now();
			retryAt = Date.now() + left / 2;
			if (left > 30000) arm(retryAt);
			throw e;
		}
	}

	/** renew resolves to a new token. refused is the token the server turned away: if another call has replaced it already, that one is returned. */
	function renew(refused) {
		if (refused !== undefined && refused !== cur.token) return Promise.resolve(cur.token);
		minting ??= mintNext().finally(() => (minting = null));
		return minting;
	}

	async function current() {
		if (!(Date.now() >= halfway() && Date.now() >= retryAt)) return cur.token;
		try {
			return await renew();
		} catch (e) {
			if (Date.now() < cur.exp) return cur.token;
			throw e;
		}
	}

	arm();
	return {
		token: current,
		renew,
		stop() {
			stopped = true;
			clearTimeout(timer);
		},
	};
}

/**
 * authFetch is a fetch for @libsql/client's config: each request carries the
 * session's token, and one the server refuses with 401 is sent again with a
 * renewed token, once. send is the fetch underneath.
 */
export function authFetch(session, send = (req) => fetch(req)) {
	return async (req) => {
		const body = req.method === "GET" || req.method === "HEAD" ? undefined : await req.arrayBuffer();
		const sign = (token) => {
			const headers = new Headers(req.headers);
			headers.set("authorization", `Bearer ${token}`);
			return send(new Request(req.url, { method: req.method, headers, body }));
		};
		const used = await session.token();
		const res = await sign(used);
		if (res.status !== 401) return res;
		await res.body?.cancel();
		return sign(await session.renew(used));
	};
}
