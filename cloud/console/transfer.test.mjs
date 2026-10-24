// The upload and download helpers against a fake server, under Node: node transfer.test.mjs
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { Code, ConnectError } from "@connectrpc/connect";
import { cancelled, downloadExport, uploadFile } from "./src/cloud/transfer.js";

const sha = (b) => createHash("sha256").update(b).digest("hex");
const data = (n) => {
	const b = new Uint8Array(n);
	for (let i = 0; i < n; i++) b[i] = (i * 31 + (i >> 8)) & 255;
	return b;
};
const file = (b, name = "app.musq") => new File([b], name);

// A server holding one upload. `fail(call, n)` may throw for the nth call of a kind, after or before the server acts.
function fakeServer({ max = 1000, held = new Uint8Array(0), limit = Infinity, fail = () => {} } = {}) {
	const s = { held, calls: { start: 0, chunk: 0, finish: 0 }, offsets: [], finished: null };
	s.db = {
		async startUpload(req, { signal } = {}) {
			signal?.throwIfAborted();
			const n = ++s.calls.start;
			fail("start", n, "before");
			if (req.sizeBytes > limit) throw new ConnectError("over the limit", Code.ResourceExhausted);
			if (req.resume && req.resume !== "up1") throw new ConnectError("unknown upload", Code.InvalidArgument);
			return { upload: "up1", offset: BigInt(s.held.length), maxChunkBytes: max };
		},
		async uploadChunk(req, { signal } = {}) {
			signal?.throwIfAborted();
			const n = ++s.calls.chunk;
			fail("chunk", n, "before");
			s.offsets.push(Number(req.offset));
			if (Number(req.offset) !== s.held.length) throw new ConnectError(`the upload holds ${s.held.length} bytes`, Code.FailedPrecondition);
			if (req.content.length > max) throw new ConnectError("chunk too large", Code.InvalidArgument);
			const next = new Uint8Array(s.held.length + req.content.length);
			next.set(s.held);
			next.set(req.content, s.held.length);
			s.held = next;
			fail("chunk", n, "after");
			return { offset: BigInt(s.held.length) };
		},
		async finishUpload(req) {
			const n = ++s.calls.finish;
			fail("finish", n, "before");
			if (Buffer.from(req.sha256).toString("hex") !== sha(s.held)) throw new ConnectError("the SHA-256 does not match", Code.InvalidArgument);
			s.finished = s.held;
			fail("finish", n, "after");
			return { upload: req.upload, format: 1, sizeBytes: BigInt(s.held.length) };
		},
	};
	return s;
}
const lost = () => new ConnectError("connection reset", Code.Unavailable);

// Several pieces, progress up to the whole.
{
	const content = data(10_500);
	const s = fakeServer();
	const seen = [];
	const r = await uploadFile(s.db, "acme", file(content), { onProgress: (a, b) => seen.push([a, b]) });
	assert.equal(r.upload, "up1");
	assert.equal(sha(s.finished), sha(content));
	assert.deepEqual(s.offsets, Array.from({ length: 11 }, (_, i) => i * 1000));
	assert.deepEqual(seen[0], [0, 10_500]);
	assert.deepEqual(seen.at(-1), [10_500, 10_500]);
	assert.ok(seen.every(([a], i) => i === 0 || a > seen[i - 1][0]));
}

// An answer lost after the server took the piece: ask, and go on from there.
{
	const content = data(5_000);
	const s = fakeServer({ fail: (kind, n, when) => { if (kind === "chunk" && n === 2 && when === "after") throw lost(); } });
	await uploadFile(s.db, "acme", file(content));
	assert.equal(sha(s.finished), sha(content));
	assert.equal(s.calls.start, 2, "one start, one ask");
	assert.equal(s.calls.chunk, 5, "no piece twice");
}

// A piece that never arrived is sent again.
{
	const content = data(3_000);
	const s = fakeServer({ fail: (kind, n, when) => { if (kind === "chunk" && n === 2 && when === "before") throw lost(); } });
	await uploadFile(s.db, "acme", file(content));
	assert.equal(sha(s.finished), sha(content));
	assert.deepEqual(s.offsets, [0, 1000, 2000]);
}

// Even when asking fails once.
{
	const content = data(3_000);
	const s = fakeServer({
		fail: (kind, n, when) => {
			if (kind === "chunk" && n === 1 && when === "after") throw lost();
			if (kind === "start" && n === 2) throw lost();
		},
	});
	await uploadFile(s.db, "acme", file(content));
	assert.equal(sha(s.finished), sha(content));
}

// A resumed upload goes on from what the server holds, and the hash covers all of it.
{
	const content = data(4_321);
	const s = fakeServer({ held: content.slice(0, 2_000) });
	await uploadFile(s.db, "acme", file(content), { resume: "up1" });
	assert.equal(sha(s.finished), sha(content));
	assert.deepEqual(s.offsets, [2000, 3000, 4000]);
	// ...and one that holds another file's start is refused by the server's hash.
	const other = fakeServer({ held: data(2_000).map((x) => x ^ 1) });
	await assert.rejects(uploadFile(other.db, "acme", file(content), { resume: "up1" }), /SHA-256/);
	await assert.rejects(uploadFile(fakeServer().db, "acme", file(content), { resume: "nosuch" }), /unknown upload/);
}

// Refusals are final: nothing is asked or sent again.
{
	const s = fakeServer({ limit: 100 });
	await assert.rejects(uploadFile(s.db, "acme", file(data(500))), (e) => ConnectError.from(e).code === Code.ResourceExhausted);
	assert.equal(s.calls.chunk, 0);
	const t = fakeServer({ fail: (kind, n) => { if (kind === "chunk" && n === 2) throw new ConnectError("no", Code.PermissionDenied); } });
	await assert.rejects(uploadFile(t.db, "acme", file(data(3_000))), (e) => ConnectError.from(e).code === Code.PermissionDenied);
	assert.equal(t.calls.chunk, 2);
	assert.equal(t.calls.start, 1);
	await assert.rejects(uploadFile(fakeServer().db, "acme", file(new Uint8Array(0))), /is empty/);
}

// A network that stays down gives up.
{
	const s = fakeServer({ fail: (kind) => { if (kind === "chunk") throw lost(); } });
	const t0 = Date.now();
	await assert.rejects(uploadFile(s.db, "acme", file(data(1_000)), { backoffMs: 1 }), (e) => ConnectError.from(e).code === Code.Unavailable);
	assert.equal(s.calls.chunk, 6);
	assert.ok(Date.now() - t0 < 5_000);
}

// Stopping stops: no more pieces, no finish.
{
	const ctl = new AbortController();
	const s = fakeServer();
	const run = uploadFile(s.db, "acme", file(data(10_000)), { signal: ctl.signal, onProgress: (a) => a >= 3000 && ctl.abort() });
	await assert.rejects(run, cancelled);
	assert.ok(s.calls.chunk <= 4, `${s.calls.chunk} pieces after stopping`);
	assert.equal(s.calls.finish, 0);
}

// The answer to the finish is lost: it is asked again, and the server answers the same.
{
	const content = data(2_500);
	const s = fakeServer({ fail: (kind, n, when) => { if (kind === "finish" && n === 1 && when === "after") throw lost(); } });
	const r = await uploadFile(s.db, "acme", file(content), { backoffMs: 1 });
	assert.equal(r.upload, "up1");
	assert.equal(s.calls.finish, 2);
	assert.equal(sha(s.finished), sha(content));
	// A refusal of the finish is final.
	const t = fakeServer({ fail: (kind, n) => { if (kind === "finish") throw new ConnectError("no", Code.PermissionDenied); } });
	await assert.rejects(uploadFile(t.db, "acme", file(content), { backoffMs: 1 }), (e) => ConnectError.from(e).code === Code.PermissionDenied);
	assert.equal(t.calls.finish, 1);
}

// A damaged upload is the server's to refuse, and says so.
{
	const s = fakeServer();
	const db = { ...s.db, uploadChunk: async (req, o) => s.db.uploadChunk({ ...req, content: req.content.map((x, i) => (i === 0 ? x ^ 255 : x)) }, o) };
	// The server's hash is over what arrived, the client's over what it read.
	await assert.rejects(uploadFile(db, "acme", file(data(2_500))), /SHA-256/);
}

// Downloads: the pieces end up as one file, with progress.
const exportOf = (parts, failAt = -1) => ({
	async *export() {
		for (const [i, p] of parts.entries()) {
			if (i === failAt) throw new ConnectError("the stream broke", Code.Unavailable);
			yield { content: p, sizeBytes: i === 0 ? BigInt(parts.reduce((n, q) => n + q.length, 0)) : 0n };
		}
	},
});
const blobBytes = async (b) => new Uint8Array(await b.arrayBuffer());
{
	const parts = [data(1000), data(500).map((x) => x ^ 1), data(7)];
	const saved = [];
	const seen = [];
	await downloadExport(exportOf(parts), { org: "acme", name: "app" }, "app.musq", { save: (b, n) => saved.push([b, n]), onProgress: (a, t) => seen.push([a, t]) });
	assert.equal(saved.length, 1);
	assert.equal(saved[0][1], "app.musq");
	assert.deepEqual(await blobBytes(saved[0][0]), Uint8Array.from(parts.flatMap((p) => [...p])));
	assert.deepEqual(seen, [[1000, 1507], [1500, 1507], [1507, 1507]]);
}

// Enough of it to fold into a Blob on the way, and nothing saved when it breaks.
{
	const parts = Array.from({ length: 4 }, (_, i) => data(20 << 20).fill(i + 1, 0, 8));
	const saved = [];
	await downloadExport(exportOf(parts), {}, "big.musq", { save: (b) => saved.push(b) });
	const b = saved[0];
	assert.equal(b.size, 80 << 20);
	assert.deepEqual(await blobBytes(b.slice(0, 8)), new Uint8Array(8).fill(1));
	assert.deepEqual(await blobBytes(b.slice(60 << 20, (60 << 20) + 8)), new Uint8Array(8).fill(4));
	const broken = [];
	await assert.rejects(downloadExport(exportOf([data(10), data(10), data(10)], 2), {}, "x", { save: (x) => broken.push(x) }), (e) => ConnectError.from(e).code === Code.Unavailable);
	assert.equal(broken.length, 0);
}

// Where the browser writes files itself, the pieces go straight there.
{
	const written = [];
	let closed = false;
	let aborted = false;
	globalThis.showSaveFilePicker = async ({ suggestedName }) => {
		assert.equal(suggestedName, "app.musq");
		return { createWritable: async () => ({ write: async (c) => written.push(c), close: async () => { closed = true; }, abort: async () => { aborted = true; } }) };
	};
	const saved = [];
	await downloadExport(exportOf([data(100), data(50)]), {}, "app.musq", { save: (b) => saved.push(b) });
	assert.equal(written.length, 2);
	assert.ok(closed && !aborted && saved.length === 0);

	closed = false;
	await assert.rejects(downloadExport(exportOf([data(10), data(10)], 1), {}, "app.musq", {}));
	assert.ok(aborted && !closed);

	// Closing the dialog is a cancel, not a failure.
	globalThis.showSaveFilePicker = async () => {
		throw new DOMException("The user aborted a request.", "AbortError");
	};
	await assert.rejects(downloadExport(exportOf([data(10)]), {}, "app.musq", {}), cancelled);

	// A page that may not show the dialog (a frame, a policy) still gets its file.
	globalThis.showSaveFilePicker = async () => {
		throw new DOMException("Cross origin sub frames aren't allowed to show a file picker.", "SecurityError");
	};
	const fallback = [];
	await downloadExport(exportOf([data(10), data(5)]), {}, "app.musq", { save: (b, n) => fallback.push([b, n]) });
	assert.equal(fallback.length, 1);
	assert.equal(fallback[0][0].size, 15);
	delete globalThis.showSaveFilePicker;
}

console.log("transfer: ok");
