// Database files to and from a musql cloud server in pieces, so the page never
// holds a whole file: an upload reads one slice of the File at a time, an
// export is written out as it arrives.
import { Code, ConnectError } from "@connectrpc/connect";

const RETRIES = 5;
// What sending again cannot change: the server refused, or the caller stopped.
const FINAL = new Set([Code.Canceled, Code.InvalidArgument, Code.PermissionDenied, Code.Unauthenticated, Code.NotFound, Code.ResourceExhausted, Code.Unimplemented]);
// Where a download without a file handle folds what it holds into a Blob.
const FOLD = 32 << 20;

/** Whether e is the caller stopping a transfer, or closing the file picker. */
export const cancelled = (e) => e?.name === "AbortError" || ConnectError.from(e).code === Code.Canceled;

const wait = (ms, signal) =>
	new Promise((resolve, reject) => {
		const t = setTimeout(resolve, ms);
		signal?.addEventListener("abort", () => { clearTimeout(t); reject(signal.reason); }, { once: true });
	});

/**
 * uploadFile sends file through db and resolves to {upload, format}, the id to
 * create a database from, with onProgress(sent, total). A piece that fails
 * like the network is resent from where the server says the upload stands,
 * and the server checks the SHA-256 at the end. resume continues an upload.
 */
export async function uploadFile(db, org, file, { onProgress, signal, resume = "", backoffMs = 500 } = {}) {
	if (!file.size) throw new Error(`${file.name} is empty`);
	const size = file.size;
	const begin = await db.startUpload({ org, sizeBytes: BigInt(size), resume }, { signal });
	const upload = begin.upload;
	const piece = begin.maxChunkBytes || 4 << 20;
	const bytes = async (from, to) => new Uint8Array(await file.slice(from, to).arrayBuffer());

	// Only an upload hashes; this keeps the library out of the page's main chunk.
	const { sha256 } = await import("@noble/hashes/sha2.js");
	const hash = sha256.create();
	let hashed = 0;
	// catchUp feeds the hash what the server holds that was not sent from here.
	const catchUp = async (to) => {
		while (hashed < to) {
			const part = await bytes(hashed, Math.min(hashed + piece, to));
			hash.update(part);
			hashed += part.length;
		}
	};
	// send returns the offset the server holds after data, which goes at off.
	const send = async (off, data) => {
		for (let attempt = 0; ; attempt++) {
			try {
				return Number((await db.uploadChunk({ org, upload, offset: BigInt(off), content: data }, { signal })).offset);
			} catch (e) {
				if (signal?.aborted || attempt === RETRIES || FINAL.has(ConnectError.from(e).code)) throw e;
			}
			await wait(backoffMs * (attempt + 1), signal);
			try {
				const at = Number((await db.startUpload({ org, sizeBytes: BigInt(size), resume: upload }, { signal })).offset);
				if (at !== off) return at;
			} catch (e) {
				if (FINAL.has(ConnectError.from(e).code)) throw e;
			}
		}
	};

	let off = Number(begin.offset);
	if (off > size) throw new Error(`the upload holds more than ${file.name} does`);
	await catchUp(off);
	onProgress?.(off, size);
	while (off < size) {
		const data = await bytes(off, Math.min(off + piece, size));
		const next = await send(off, data);
		if (next === off + data.length) {
			hash.update(data);
			hashed = next;
		} else if (next < off || next > size) {
			throw new Error("the server holds a part of the upload that is not where it left off");
		} else {
			await catchUp(next);
		}
		off = next;
		onProgress?.(off, size);
	}
	// Finishing is idempotent, so an answer that was lost is asked for again.
	const sum = hash.digest();
	for (let attempt = 0; ; attempt++) {
		try {
			const done = await db.finishUpload({ org, upload, sha256: sum }, { signal });
			return { upload: done.upload, format: done.format };
		} catch (e) {
			if (signal?.aborted || attempt === RETRIES || FINAL.has(ConnectError.from(e).code)) throw e;
		}
		await wait(backoffMs * (attempt + 1), signal);
	}
}

function saveBlob(blob, filename) {
	const a = document.createElement("a");
	a.href = URL.createObjectURL(blob);
	a.download = filename;
	a.click();
	setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}

/**
 * downloadExport saves an export as filename, with onProgress(received,
 * total). Where the browser can write a file as pieces arrive it holds none of
 * it; otherwise the pieces fold into a Blob. Call it from the click that starts
 * the download: the save dialog needs that click.
 */
export async function downloadExport(db, req, filename, { onProgress, signal, save = saveBlob } = {}) {
	let out = null;
	if (typeof showSaveFilePicker === "function") {
		try {
			out = await (await showSaveFilePicker({ suggestedName: filename })).createWritable();
		} catch (e) {
			// Closing the dialog is the user's answer; a page that may not show it still gets its file.
			if (e?.name === "AbortError") throw e;
		}
	}
	let blob = new Blob([]);
	let parts = [];
	let held = 0;
	let received = 0;
	let total = 0;
	try {
		for await (const m of db.export(req, { signal })) {
			total ||= Number(m.sizeBytes);
			received += m.content.length;
			if (out) {
				await out.write(m.content);
			} else {
				parts.push(m.content);
				held += m.content.length;
				if (held >= FOLD) {
					blob = new Blob([blob, ...parts]);
					parts = [];
					held = 0;
				}
			}
			onProgress?.(received, total);
		}
		if (out) await out.close();
		else save(new Blob([blob, ...parts]), filename);
	} catch (e) {
		await out?.abort().catch(() => {});
		throw e;
	}
}
