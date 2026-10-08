// An in-memory filesystem with the slice of Node's callback fs API that Go's
// js/wasm syscall package calls (src/syscall/fs_js.go). Load it before
// wasm_exec.js: Go's os package then works unchanged in a browser, so the
// engine's files, temp files and locks (internal/filelock/filelock_js.go)
// all live here, private to this Go instance.
(() => {
	const S_IFDIR = 0o040000, S_IFREG = 0o100000;
	const O = { O_WRONLY: 1, O_RDWR: 2, O_CREAT: 64, O_EXCL: 128, O_TRUNC: 512, O_APPEND: 1024, O_DIRECTORY: 65536 };
	const nodes = new Map(); // normalized path -> node
	const fds = new Map(); // fd -> { node, pos, flags }
	let nextIno = 1, nextFd = 100;
	// mtimes in ns, strictly increasing: the engine's change check compares
	// size and mtime, so two writes in one millisecond must not share one.
	let lastNs = 0n;
	const nowNs = () => { const t = BigInt(Date.now()) * 1000000n; lastNs = t > lastNs ? t : lastNs + 1n; return lastNs; };

	const fail = (code) => { const e = new Error(code); e.code = code; return e; };
	const norm = (p) => {
		const out = [];
		for (const part of String(p).split("/")) {
			if (part === "" || part === ".") continue;
			if (part === "..") out.pop(); else out.push(part);
		}
		return "/" + out.join("/");
	};
	const parent = (p) => p === "/" ? "/" : norm(p + "/..");
	const mkdirNode = (p, mode) => nodes.set(p, { dir: true, ino: nextIno++, mode: S_IFDIR | (mode & 0o777), mtime: nowNs() });
	mkdirNode("/", 0o755);
	mkdirNode("/tmp", 0o777);

	const stat = (n) => ({
		dev: 1, ino: n.ino, mode: n.mode, nlink: n.unlinked ? 0 : 1, uid: 0, gid: 0, rdev: 0,
		size: n.dir ? 0 : n.size, blksize: 4096, blocks: n.dir ? 0 : Math.ceil(n.size / 512),
		atimeMs: Number(n.mtime / 1000000n), mtimeMs: Number(n.mtime / 1000000n), ctimeMs: Number(n.mtime / 1000000n),
		isDirectory: () => !!n.dir,
	});
	const grow = (n, size) => {
		if (size > n.data.length) {
			const d = new Uint8Array(Math.max(size, n.data.length * 2, 4096));
			d.set(n.data.subarray(0, n.size));
			n.data = d;
		}
	};
	const setSize = (n, size) => {
		grow(n, size);
		if (size < n.size) n.data.fill(0, size, n.size);
		n.size = size;
		n.mtime = nowNs();
	};
	const lookup = (p) => { const n = nodes.get(norm(p)); if (!n) throw fail("ENOENT"); return n; };
	const fdOf = (fd) => { const f = fds.get(fd); if (!f) throw fail("EBADF"); return f; };
	// Every call answers through its callback; a throw becomes the error.
	const cb = (fn) => (...args) => {
		const done = args.pop();
		let res;
		try { res = fn(...args); } catch (e) { done(e); return; }
		done(null, res);
	};

	const fs = {
		constants: O,
		open: cb((path, flags, mode) => {
			const p = norm(path);
			let n = nodes.get(p);
			if (n && (flags & O.O_CREAT) && (flags & O.O_EXCL)) throw fail("EEXIST");
			if (!n) {
				if (!(flags & O.O_CREAT)) throw fail("ENOENT");
				const dir = nodes.get(parent(p));
				if (!dir) throw fail("ENOENT");
				if (!dir.dir) throw fail("ENOTDIR");
				n = { ino: nextIno++, mode: S_IFREG | (mode & 0o777), data: new Uint8Array(0), size: 0, mtime: nowNs() };
				nodes.set(p, n);
			}
			if ((flags & O.O_DIRECTORY) && !n.dir) throw fail("ENOTDIR");
			if (n.dir && (flags & (O.O_WRONLY | O.O_RDWR))) throw fail("EISDIR");
			if ((flags & O.O_TRUNC) && !n.dir) setSize(n, 0);
			const fd = nextFd++;
			fds.set(fd, { node: n, pos: 0, flags });
			return fd;
		}),
		close: cb((fd) => { fdOf(fd); fds.delete(fd); }),
		fstat: cb((fd) => stat(fdOf(fd).node)),
		stat: cb((path) => stat(lookup(path))),
		lstat: cb((path) => stat(lookup(path))),
		read: cb((fd, buf, off, len, pos) => {
			const f = fdOf(fd), n = f.node;
			if (n.dir) throw fail("EISDIR");
			const at = pos === null || pos === undefined ? f.pos : Number(pos);
			const k = Math.max(0, Math.min(len, n.size - at));
			buf.set(n.data.subarray(at, at + k), off);
			if (pos === null || pos === undefined) f.pos += k;
			return k;
		}),
		write: cb((fd, buf, off, len, pos) => {
			const f = fdOf(fd), n = f.node;
			let at = pos === null || pos === undefined ? f.pos : Number(pos);
			if (f.flags & O.O_APPEND) at = n.size;
			grow(n, at + len);
			n.data.set(buf.subarray(off, off + len), at);
			if (at + len > n.size) n.size = at + len;
			n.mtime = nowNs();
			if (pos === null || pos === undefined) f.pos = at + len;
			return len;
		}),
		fsync: cb(() => {}),
		ftruncate: cb((fd, len) => setSize(fdOf(fd).node, Number(len))),
		truncate: cb((path, len) => setSize(lookup(path), Number(len))),
		mkdir: cb((path, mode) => {
			const p = norm(path);
			if (nodes.has(p)) throw fail("EEXIST");
			if (!nodes.get(parent(p))?.dir) throw fail("ENOENT");
			mkdirNode(p, mode);
		}),
		readdir: cb((path) => {
			const p = norm(path);
			if (!lookup(p).dir) throw fail("ENOTDIR");
			const pre = p === "/" ? "/" : p + "/";
			const names = [];
			for (const k of nodes.keys()) {
				if (k !== p && k.startsWith(pre) && !k.slice(pre.length).includes("/")) names.push(k.slice(pre.length));
			}
			return names;
		}),
		unlink: cb((path) => {
			const p = norm(path);
			const n = lookup(p);
			if (n.dir) throw fail("EISDIR");
			n.unlinked = true; // open descriptors keep it, with no name: nlink 0
			nodes.delete(p);
		}),
		rmdir: cb((path) => {
			const p = norm(path);
			if (!lookup(p).dir) throw fail("ENOTDIR");
			for (const k of nodes.keys()) if (k.startsWith(p + "/")) throw fail("ENOTEMPTY");
			nodes.delete(p);
		}),
		rename: cb((from, to) => {
			const a = norm(from), b = norm(to);
			const n = lookup(a);
			if (!nodes.get(parent(b))?.dir) throw fail("ENOENT");
			const moved = [[a, n]];
			if (n.dir) for (const [k, v] of nodes) if (k.startsWith(a + "/")) moved.push([k, v]);
			for (const [k] of moved) nodes.delete(k);
			for (const [k, v] of moved) {
				const old = nodes.get(b + k.slice(a.length));
				if (old && old !== v) old.unlinked = true; // replaced: its descriptors now name no path
				nodes.set(b + k.slice(a.length), v);
			}
		}),
		utimes: cb((path, atime, mtime) => { lookup(path).mtime = BigInt(Math.round(mtime * 1e9)); }),
		chmod: cb((path, mode) => { const n = lookup(path); n.mode = (n.mode & ~0o777) | (mode & 0o777); }),
		fchmod: cb((fd, mode) => { const n = fdOf(fd).node; n.mode = (n.mode & ~0o777) | (mode & 0o777); }),
		chown: cb(() => {}), fchown: cb(() => {}), lchown: cb(() => {}),
		link: cb(() => { throw fail("ENOSYS"); }),
		symlink: cb(() => { throw fail("ENOSYS"); }),
		readlink: cb(() => { throw fail("EINVAL"); }),

		// wasm_exec.js prints stdout/stderr through writeSync.
		writeSync(fd, buf) {
			if (fd === 1 || fd === 2) {
				(fd === 1 ? console.log : console.error)(new TextDecoder().decode(buf).replace(/\n$/, ""));
				return buf.length;
			}
			let n;
			fs.write(fd, buf, 0, buf.length, null, (e, k) => { if (e) throw e; n = k; });
			return n;
		},
		// Synchronous helpers for the page's own use: put a file in, take one out.
		writeFileSync(path, bytes) {
			const p = norm(path);
			const old = nodes.get(p);
			if (old) old.unlinked = true;
			nodes.set(p, { ino: nextIno++, mode: S_IFREG | 0o644, data: new Uint8Array(bytes), size: bytes.byteLength, mtime: nowNs() });
		},
		// Positioned read and write for the engine's commit path (internal/fsio):
		// read and write above at an explicit position, called directly.
		preadSync(fd, dst, pos) {
			const f = fds.get(fd);
			if (!f || f.node.dir) return -1;
			const n = f.node, k = Math.max(0, Math.min(dst.length, n.size - pos));
			dst.set(n.data.subarray(pos, pos + k));
			return k;
		},
		pwriteSync(fd, src, pos) {
			const f = fds.get(fd);
			if (!f || f.node.dir) return -1;
			const n = f.node;
			const at = f.flags & O.O_APPEND ? n.size : pos;
			grow(n, at + src.length);
			n.data.set(src, at);
			if (at + src.length > n.size) n.size = at + src.length;
			n.mtime = nowNs();
			return src.length;
		},
		fsyncSync(fd) { return fds.has(fd) ? 0 : -1; },
		// fstat for the engine's held-descriptor stamp (internal/fsstamp OfFile):
		// size, mtime in ns, and whether the file still has a name.
		fstatStamp(fd) { const f = fds.get(fd); if (!f) return null; const n = f.node; return [n.dir ? 0 : n.size, n.mtime, n.unlinked ? 0 : 1]; },
		statStamp(path) { const n = nodes.get(norm(path)); return n ? [n.dir ? 0 : n.size, n.mtime] : null; },
		readFileSync(path) { const n = lookup(path); return n.data.slice(0, n.size); },
	};
	// Writes to stdout/stderr come through write() with fd 1 or 2 too.
	const write = fs.write;
	fs.write = (fd, buf, off, len, pos, done) => {
		if (fd === 1 || fd === 2) { done(null, fs.writeSync(fd, buf.subarray(off, off + len))); return; }
		write(fd, buf, off, len, pos, done);
	};

	globalThis.fs = fs;
	globalThis.process ??= {
		getuid: () => -1, getgid: () => -1, geteuid: () => -1, getegid: () => -1,
		getgroups: () => [], pid: -1, ppid: -1, umask: () => 0o022,
		cwd: () => "/", chdir: () => { throw fail("ENOSYS"); },
	};
})();
