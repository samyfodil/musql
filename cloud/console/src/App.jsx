import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { backend, connect, exportMusql, exportSQLite, hostOf, isRemote, onBeforeWrite, open, query, ready } from "./db.js";
import { forgetRemote, keepRemote, remoteNamed, remotes } from "./remotes.js";
import { dumpSQL } from "./dump.js";
import { forgetDb, listDbs, loadDb, saveDb } from "./store.js";
import { viewState } from "./view.js";
import { verb } from "./sqltext.js";
import { fmt } from "./values.js";
import { Studio } from "./studio.js";
import { BusyBar } from "./components/Busy.jsx";
import { MenuButton } from "./components/Menu.jsx";
import { useToast } from "./components/Toasts.jsx";
import Palette from "./components/Palette.jsx";
import Shortcuts from "./components/Shortcuts.jsx";
import { Cloud, Contrast, Globe, HardDrive, Keyboard, Moon, Network, Search, SquareTerminal, Sun, Table2 } from "lucide-react";
import ConnectDrawer from "./components/ConnectDrawer.jsx";
import { SAMPLE } from "./sample.js";
import { CloudMain, CloudSide, cloudPages } from "./cloud/Cloud.jsx";
import { useCloud } from "./cloud/useCloud.js";
import { server as cloudServer, signIn, token as accessToken } from "./cloud/auth.js";
import { api, cloudDatabase, roleName, transport } from "./cloud/api.js";
import Logo, { slotIcon, SlotBoundary } from "./components/Logo.jsx";
import { Slot, useOptions } from "./slots.js";
import { useTheme } from "./theme.jsx";
import { ICON, useWorkspace } from "./workspace/useWorkspace.js";
import { WorkspaceDialogs, WorkspaceEmpty, WorkspacePanes, WorkspaceSide, WorkspaceTabs } from "./workspace/parts.jsx";

const THEMES = { system: <Contrast size={17} />, light: <Sun size={17} />, dark: <Moon size={17} /> };


export default function App() {
	const opts = useOptions();
	const { branding } = opts;
	const [theme, setTheme, dark] = useTheme();
	const [dirty, setDirty] = useState(false);
	const [dragging, setDragging] = useState(false);
	const [cloudPage, setCloudPage] = useState("databases");
	const cloud = useCloud();
	const [palette, setPalette] = useState(false);
	const [keys, setKeys] = useState(false);
	const [sideOpen, setSideOpen] = useState(false); // the sidebar, on a narrow screen
	const [savedAt, setSavedAt] = useState(null);
	const [recents, setRecents] = useState([]);
	const [hist, setHist] = useState({ undo: 0, redo: 0 });
	const [conn, setConn] = useState(backend()); // what is open: local, or a remote host
	const [remoteList, setRemoteList] = useState(remotes);
	const [connecting, setConnecting] = useState(null); // a ConnectDrawer's starting values, or null

	const toast = useToast();

	// ---- undo, redo and saving ----
	// Before a change's first write the database is snapshotted (an export, a
	// few ms), and undo puts the snapshot back whole, exact for any change.
	// Writes within half a second or in one transaction are one change, saved
	// in this browser once it settles.
	const undoStack = useRef([]);
	const redoStack = useRef([]);
	const grouping = useRef(false);
	const inTxn = useRef(false);
	const lastStep = useRef(0);
	const saveTimer = useRef(null);
	const nameRef = useRef(null);
	const syncHist = () => setHist({ undo: undoStack.current.length, redo: redoStack.current.length });

	const saveSoon = useCallback((delay = 600) => {
		clearTimeout(saveTimer.current);
		saveTimer.current = setTimeout(async () => {
			if (!nameRef.current || inTxn.current || isRemote()) return;
			try {
				const bytes = await exportMusql();
				await saveDb(nameRef.current, bytes);
				setSavedAt(Date.now());
			} catch {
				// a database mid-transaction exports later
			}
		}, delay);
	}, []);

	useEffect(() => {
		onBeforeWrite(async (sql) => {
			// A remote database has no snapshot to take: undo is local-only.
			if (isRemote()) return;
			const v = verb(sql);
			if (v === "COMMIT" || v === "END" || v === "ROLLBACK") {
				inTxn.current = false;
				saveSoon();
				return;
			}
			const describe = () => {
				const what = /^\s*(\w+)(?:\s+(?:OR\s+\w+\s+)?(?:INTO|FROM|TABLE|INDEX|VIEW|TRIGGER))?\s+"?([\w$]+)?/i.exec(sql.replace(/^(\s|--[^\n]*\n)*/, ""));
				return what?.[2] ? `${what[1].toLowerCase()} ${what[2]}` : v.toLowerCase();
			};
			// A step opened by BEGIN or SAVEPOINT is named after the first statement in it.
			const top = undoStack.current.at(-1);
			const opener = v === "BEGIN" || v === "SAVEPOINT";
			if (top?.vague && (inTxn.current || grouping.current) && !opener) {
				top.label = describe();
				top.vague = false;
			}
			if (inTxn.current) return;
			if (!grouping.current) {
				const snap = await exportMusql();
				undoStack.current.push({ label: opener ? "change" : describe(), snap, vague: opener });
				let bytes = undoStack.current.reduce((n, x) => n + x.snap.length, 0);
				while (undoStack.current.length > 1 && (undoStack.current.length > 50 || bytes > 256e6)) bytes -= undoStack.current.shift().snap.length;
				redoStack.current = [];
				lastStep.current = Date.now();
				grouping.current = true;
				setTimeout(() => (grouping.current = false), 500);
				syncHist();
			}
			if (v === "BEGIN" || v === "SAVEPOINT") inTxn.current = true;
			saveSoon();
		});
		return () => onBeforeWrite(null);
	}, [saveSoon]);

	const loadSchemaRef = useRef(null);
	const swap = useCallback(async (from, to, verbWord) => {
		if (isRemote()) return toast("Undo works on local databases, not on a remote one.", true);
		if (inTxn.current) return toast("Finish the open transaction first.", true);
		const step = from.current.pop();
		if (!step) return;
		const now = await exportMusql();
		to.current.push({ label: step.label, snap: now });
		await open(step.snap);
		await loadSchemaRef.current();
		ws.setEpoch((e) => e + 1);
		syncHist();
		saveSoon(0);
		toast(`${verbWord} ${step.label}`);
	}, [toast, saveSoon]);
	const undo = useCallback(() => swap(undoStack, redoStack, "Undid"), [swap]);
	const redo = useCallback(() => swap(redoStack, undoStack, "Redid"), [swap]);
	const undoRef = useRef(undo);
	undoRef.current = undo;

	// A message right after a change offers to undo it.
	const status = useCallback((text, err) => {
		if (/^Opening /.test(text) && !err) return;
		const fresh = !err && Date.now() - lastStep.current < 2500 && undoStack.current.length;
		toast(text, err, fresh ? { label: "Undo", run: () => undoRef.current() } : undefined);
	}, [toast]);

	// The rail: "tables" | "sql" | "diagram" | "cloud".
	const ws = useWorkspace({ status, dark, onChanged: () => setDirty(true), initialSection: cloudServer && (location.hash === "#cloud" || location.pathname === "/console/callback") ? "cloud" : "tables", cloudSection: true });
	const { name: dbName, schema, section, setSection, tabs, openTable, openQuery, openDiagram, openTab, setTabs, setActive, internal, current } = ws;
	nameRef.current = dbName;
	loadSchemaRef.current = ws.loadSchema;

	// ---- databases ----

	// openDatabase opens bytes (or a new database) as name. A database is kept
	// in this browser from then on; reopening a kept one restores its tabs.
	// remote, when given, is a kept connection ({url, token}) to open instead.
	async function openDatabase(name, data, then, route, remote) {
		try {
			const t = performance.now();
			if (!data && !then && !remote) name = await freeName(name);
			// Read the kept view before anything renders under the new name: the
			// effect that keeps it would otherwise save an empty one first.
			const view = viewState(name);
			if (remote) await connect(remote.url, remote.token, remote.renewal);
			else await open(data);
			setConn(backend());
			setSavedAt(null);
			if (then) await query(then);
			undoStack.current = [];
			redoStack.current = [];
			syncHist();
			nameRef.current = name;
			setDirty(false);
			const s = await ws.loadSchema();
			saveSoon(0);
			try {
				localStorage.setItem("musql-studio:last", name);
			} catch {
				// not remembered across reloads
			}
			const restored = ws.show(name, s, view);
			if (route) applyRoute(route, s);
			else if (!restored) ws.openFirst(s);
			if (!route) status(`Opened ${name} in ${(performance.now() - t).toFixed(0)} ms`);
			listDbs().then(setRecents);
		} catch (e) {
			status(`Could not open ${name}: ${e.message}`, true);
		}
	}

	// freeName is name, or name with a number, so a new database never
	// replaces a kept one.
	async function freeName(name) {
		const have = new Set((await listDbs()).map((d) => d.name));
		if (!have.has(name)) return name;
		const [, base, ext] = /^(.*?)(\.[^.]*)?$/.exec(name);
		for (let i = 2; ; i++) if (!have.has(`${base}-${i}${ext ?? ""}`)) return `${base}-${i}${ext ?? ""}`;
	}

	// ---- the URL ----
	// #/<database>/table/<name>?s=<state>, #/<database>/sql/<title> or
	// #/<database>/diagram: the open database and tab, a table's filter, sort
	// and view, so a link reopens it and Back and Forward move between tabs.
	function routeOf(t) {
		if (!dbName || !t || t.kind === "ext") return dbName ? `#/${encodeURIComponent(dbName)}` : "#/";
		const base = `#/${encodeURIComponent(dbName)}`;
		const st = t.state;
		const custom = st && (st.mode !== "data" || st.filt?.filters?.length || st.filt?.sql || st.sorts?.length);
		if (t.kind === "table") return `${base}/table/${encodeURIComponent(t.table)}${custom ? `?s=${encodeURIComponent(JSON.stringify(st))}` : ""}`;
		if (t.kind === "query") return `${base}/sql/${encodeURIComponent(t.title)}`;
		return `${base}/diagram`;
	}
	function parseRoute(hash) {
		const m = /^#\/([^/?]*)(?:\/(table|sql|diagram)(?:\/([^?]*))?)?(?:\?s=(.*))?$/.exec(hash);
		if (!m || !m[1]) return null;
		let state;
		try {
			state = m[4] ? JSON.parse(decodeURIComponent(m[4])) : undefined;
		} catch {
			state = undefined;
		}
		return { db: decodeURIComponent(m[1]), kind: m[2], name: m[3] ? decodeURIComponent(m[3]) : undefined, state };
	}
	function applyRoute(r, s = schema) {
		if (r.kind === "table" && s.some((o) => o.name === r.name)) {
			setSection("tables");
			openTable(r.name, r.state?.mode ?? "data", r.state);
		} else if (r.kind === "sql") {
			setSection("sql");
			setTabs((ts) => {
				const q = ts.find((t) => t.kind === "query" && t.title === r.name);
				if (q) setActive(q.id);
				return ts;
			});
		} else if (r.kind === "diagram") {
			setSection("diagram");
			openDiagram();
		}
	}

	async function openSaved(name, route) {
		const r = remoteNamed(name);
		if (r) return openDatabase(name, undefined, undefined, route, r);
		const bytes = await loadDb(name);
		if (!bytes) return status(`${name} is no longer kept in this browser.`, true);
		await openDatabase(name, bytes, undefined, route);
	}

	async function openFile(file) {
		if (!file) return;
		if (/\.(csv|tsv|json)$/i.test(file.name)) {
			if (!dbName) await openDatabase("untitled.musq");
			ws.setDialog({ import: file });
			return;
		}
		await openDatabase(file.name, new Uint8Array(await file.arrayBuffer()));
	}

	async function exportAs(kind) {
		if (!dbName) return status("Nothing to export yet.", true);
		try {
			const bytes = kind === "sql" ? new TextEncoder().encode(await dumpSQL()) : await (kind === "sqlite" ? exportSQLite() : exportMusql());
			const a = document.createElement("a");
			a.href = URL.createObjectURL(new Blob([bytes]));
			a.download = `${dbName.replace(/\.(musq|sqlite3?|db)$/i, "")}.${kind === "sqlite" ? "sqlite" : kind === "sql" ? "sql" : "musq"}`;
			a.click();
			setTimeout(() => URL.revokeObjectURL(a.href), 1000);
			setDirty(false);
			status(`Exported ${a.download} · ${fmt(Math.ceil(bytes.length / 1024))} KB`);
		} catch (e) {
			status(`Export failed: ${e.message}`, true);
		}
	}

	// Startup: the database the URL names, or the last one open, if kept.
	useEffect(() => {
		ready.then(async () => {
			listDbs().then(setRecents);
			const r = parseRoute(location.hash);
			let last = null;
			try {
				last = localStorage.getItem("musql-studio:last");
			} catch {
				last = null;
			}
			const name = r?.db ?? last;
			if (name && (remoteNamed(name) || (await loadDb(name)))) await openSaved(name, r?.db === name ? r : null);
		}, () => {});
	}, []);

	// The URL follows the tab in view; useWorkspace keeps the open tabs per database.
	const current0 = tabs.find((t) => t.id === ws.active);
	const lastHash = useRef(null);
	useEffect(() => {
		if (!dbName) return;
		const h = routeOf(current0);
		if (h === location.hash) return;
		const sameTab = lastHash.current && lastHash.current.split("?")[0] === h.split("?")[0];
		if (sameTab) history.replaceState(null, "", h);
		else history.pushState(null, "", h);
		lastHash.current = h;
	}, [tabs, ws.active, section, dbName]);
	useEffect(() => {
		const on = () => {
			const r = parseRoute(location.hash);
			lastHash.current = location.hash;
			if (!r) return;
			if (r.db !== nameRef.current) openSaved(r.db, r);
			else applyRoute(r);
		};
		addEventListener("popstate", on);
		return () => removeEventListener("popstate", on);
	});

	// ⌘Z and ⇧⌘Z undo and redo, except while typing in a field or the editor;
	// ? shows the shortcuts.
	useEffect(() => {
		const key = (e) => {
			if (e.key === "?" && !e.target.closest?.("input, textarea, select, .cm-editor, [contenteditable]")) {
				e.preventDefault();
				setKeys((k) => !k);
				return;
			}
			if (!(e.metaKey || e.ctrlKey) || e.key.toLowerCase() !== "z") return;
			if (e.target.closest?.("input, textarea, select, .cm-editor, [contenteditable]")) return;
			e.preventDefault();
			if (e.shiftKey) redo();
			else undo();
		};
		addEventListener("keydown", key);
		return () => removeEventListener("keydown", key);
	}, [undo, redo]);

	useEffect(() => {
		const key = (e) => {
			if (!(e.metaKey || e.ctrlKey)) return;
			if (e.key.toLowerCase() === "o") {
				e.preventDefault();
				document.getElementById("file").click();
			} else if (e.key.toLowerCase() === "k") {
				e.preventDefault();
				setPalette((p) => !p);
			}
		};
		addEventListener("keydown", key);
		return () => removeEventListener("keydown", key);
	}, []);

	const pickFile = () => document.getElementById("file").click();
	const ago = (t) => {
		const m = Math.round((Date.now() - t) / 60000);
		return m < 1 ? "just now" : m < 60 ? `${m} min ago` : m < 1440 ? `${Math.round(m / 60)} h ago` : new Date(t).toLocaleDateString();
	};
	const remote = conn.kind === "remote";
	const cloudTarget = cloudDatabase(conn);
	const others = recents.filter((r) => r.name !== dbName && !remoteNamed(r.name)).slice(0, 6);
	const otherRemotes = remoteList.filter((r) => r.name !== dbName).slice(0, 6);
	// connectRemote opens c and keeps it; it reports whether that worked, so
	// the drawer stays open on a wrong URL or token.
	const connectRemote = async (c) => {
		await openDatabase(c.name, undefined, undefined, null, c);
		if (!isRemote() || nameRef.current !== c.name) return false;
		keepRemote(c);
		setRemoteList(remotes());
		setConnecting(null);
		return true;
	};
	// openCloud opens a cloud database over Hrana with a short-lived token the
	// cloud page minted and renews; it is not kept in the connection list.
	// renamedFrom keeps a renamed database's tabs and the page where it is.
	const openCloud = async (c, renamedFrom) => {
		const view = renamedFrom && viewState(renamedFrom);
		if (view) viewState(c.name, view);
		await openDatabase(c.name, undefined, undefined, null, c);
		if (!renamedFrom && backend().url === c.url) setSection("tables");
	};
	const goCloud = (page) => {
		setSection("cloud");
		if (page) setCloudPage(page);
	};
	const openExtTab = (key) => {
		const d = opts.databaseTabs.find((t) => t.key === key);
		if (!d || !cloudTarget) return;
		setSection("tables");
		openTab({ kind: "ext", key, title: d.label });
	};
	// slot is what an extension's page, tab or banner gets as ctx.
	const membership = cloud.me?.memberships.find((m) => m.org.slug === cloud.org);
	const slot = useMemo(
		() => ({
			api,
			transport,
			server: cloudServer,
			token: accessToken,
			user: cloud.me?.user ?? null,
			admin: cloud.admin,
			org: membership?.org ?? null,
			role: roleName(cloud.role),
			orgs: cloud.me?.memberships.map((m) => m.org) ?? [],
			database: cloudTarget,
			toast,
			navigate: goCloud,
			openTab: openExtTab,
			branding,
		}),
		[cloud.me, cloud.org, cloud.role, cloud.admin, cloudTarget?.org, cloudTarget?.database, toast, branding],
	);
	const fileItems = [
		{ label: "Undo", hint: remote ? "local only" : "⌘Z", disabled: remote || !hist.undo, onClick: undo },
		{ label: "Redo", hint: remote ? "local only" : "⇧⌘Z", disabled: remote || !hist.redo, onClick: redo },
		"-",
		...others.map((r) => ({ label: r.name, hint: ago(r.at), onClick: () => openSaved(r.name) })),
		...otherRemotes.map((r) => ({ label: r.name, hint: hostOf(r.url), onClick: () => openSaved(r.name) })),
		others.length + otherRemotes.length > 0 && "-",
		{ label: "New empty database", onClick: () => openDatabase("untitled.musq") },
		{ label: "Open a file…", hint: "⌘O", onClick: pickFile },
		{ label: "Open the sample", onClick: () => openDatabase("sample.musq", undefined, SAMPLE) },
		{ label: "Connect to a remote database…", hint: "libSQL / Hrana", onClick: () => setConnecting({}) },
		remote && { label: "Edit this connection…", onClick: () => setConnecting(remoteNamed(dbName) ?? {}) },
		"-",
		remote
			? { label: "Export as a file", hint: "local only", disabled: true }
			: { label: "Export as SQLite", hint: ".sqlite", disabled: !dbName, onClick: () => exportAs("sqlite") },
		!remote && { label: "Export as musql", hint: ".musq", disabled: !dbName, onClick: () => exportAs("musql") },
		{ label: "Download as SQL dump", hint: ".sql", disabled: !dbName, onClick: () => exportAs("sql") },
	];

	// Everything the palette finds: objects first, then actions.
	const commands = useMemo(() => {
		const out = ws.commands();
		const act = (label, run, hint, icon = "→") => out.push({ group: "Actions", label, run, hint, icon });
		if (dbName) {
			if (!remote) act("Export as SQLite file", () => exportAs("sqlite"), ".sqlite", "⇩");
			if (!remote) act("Export as musql file", () => exportAs("musql"), ".musq", "⇩");
			act("Download as SQL dump", () => exportAs("sql"), ".sql", "⇩");
		}
		if (dbName && !remote) {
			act("Undo", undo, "⌘Z", "↶");
			act("Redo", redo, "⇧⌘Z", "↷");
		}
		for (const r of recents) if (r.name !== dbName && !remoteNamed(r.name)) act(`Open ${r.name}`, () => openSaved(r.name), "kept in this browser", "◷");
		for (const r of remoteList) if (r.name !== dbName) act(`Open ${r.name}`, () => openSaved(r.name), hostOf(r.url), "◎");
		act("Connect to a remote database", () => setConnecting({}), "libSQL / Hrana", "◎");
		act("Open a file", pickFile, "⌘O", "⇧");
		act("Open the sample database", () => openDatabase("sample.musq", undefined, SAMPLE), "", "★");
		act("New empty database", () => openDatabase("untitled.musq"), "", "＋");
		if (cloudServer) for (const [k, label, , admin] of cloudPages(opts)) if (!admin || cloud.admin) act(`Cloud: ${label}`, () => goCloud(k), "", "☁");
		if (!cloudServer) act(`Sign in to a ${branding.name}`, () => goCloud(), "", "☁");
		if (cloudTarget) for (const d of opts.databaseTabs) act(d.label, () => openExtTab(d.key), cloudTarget.database, "◆");
		if (branding.docsURL) act("Documentation", () => window.open(branding.docsURL, "_blank", "noopener"), "", "?");
		if (branding.supportURL) act("Support", () => window.open(branding.supportURL, "_blank", "noopener"), "", "?");
		act("Keyboard shortcuts", () => setKeys(true), "?", "⌨");
		act(`Theme: ${theme === "dark" ? "light" : "dark"}`, () => setTheme(dark ? "light" : "dark"), "", dark ? "☀" : "☾");
		return out;
	}, [schema, internal, ws.counts, ws.library, dbName, theme, dark, recents, remoteList, remote, undo, redo, cloud.admin, cloudTarget?.org, cloudTarget?.database]);

	function goSection(sec) {
		// On a narrow screen the list is an overlay a rail button opens and closes.
		setSideOpen(matchMedia("(max-width: 760px)").matches && (sec !== section || !sideOpen) && sec !== "diagram");
		ws.enter(sec);
	}

	return (
		<Studio.Provider value={ws.ctx}><Slot.Provider value={slot}>
			<div
				className={`app${sideOpen ? " sideopen" : ""}`}
				onDragEnter={(e) => e.dataTransfer.types.includes("Files") && setDragging(true)}
				onDragOver={(e) => e.preventDefault()}
				onDrop={(e) => {
					e.preventDefault();
					setDragging(false);
					openFile(e.dataTransfer.files[0]);
				}}
			>
				<BusyBar />
				<input id="file" type="file" hidden accept=".musq,.sqlite,.sqlite3,.db,.csv,.tsv,.json" onChange={(e) => { openFile(e.target.files[0]); e.target.value = ""; }} />

				<nav className="rail">
					<Logo title={branding.studioName} />
					{[
						["tables", <Table2 size={18} />, "Table editor"],
						["sql", <SquareTerminal size={18} />, "SQL editor"],
						["diagram", <Network size={18} />, "Schema diagram"],
						["cloud", <Cloud size={18} />, cloudServer ? "Cloud" : `Sign in to a ${branding.name}`],
					].map(([k, ic, label]) => (
						<button key={k} className={`railbtn${section === k ? " on" : ""}`} title={label} disabled={!dbName && k !== "tables" && k !== "cloud"} onClick={() => goSection(k)}>
							{ic}
						</button>
					))}
					<span className="grow" />
					<button className="railbtn" title="Search everything (⌘K)" onClick={() => setPalette(true)}><Search size={17} /></button>
					<button className="railbtn" title="Keyboard shortcuts (?)" onClick={() => setKeys(true)}><Keyboard size={17} /></button>
					<button className="railbtn" title={`Theme: ${theme} (click to change)`} onClick={() => setTheme(theme === "system" ? "light" : theme === "light" ? "dark" : "system")}>{THEMES[theme]}</button>
				</nav>

				<aside className="side">
					<div className="dbswitch">
						<MenuButton
							className="dbbtn"
							label={<>
								{dbName && (remote ? <Globe size={14} className="dbkind" /> : <HardDrive size={14} className="dbkind" />)}
								<span className="dbn">{dbName ?? "No database"}</span>
								{dbName && (remote
									? <span className="savestate remote" title={`Every change is written to ${conn.url} as it is made.`}>{conn.host}</span>
									: <span className={`savestate${savedAt ? " ok" : ""}`} title={savedAt ? `Local, saved in this browser ${ago(savedAt)}. Export to keep a file.` : "Saving…"}>{savedAt ? "local · saved" : "local …"}</span>)}
								<span className="dim">▾</span>
							</>}
							items={fileItems}
						/>
						{dbName && remote && <p className="dbnote">Remote: changes go straight to the server. Undo works on local databases.</p>}
					</div>
					{section === "cloud" ? (
						<CloudSide cloud={cloud} page={cloudPage} setPage={(p) => { setCloudPage(p); setSideOpen(false); }} toast={toast} />
					) : (
						<WorkspaceSide
							ws={ws}
							onPick={() => setSideOpen(false)}
							extra={cloudTarget && opts.databaseTabs.length > 0 && (
								<div className="group">
									<h4>Database</h4>
									{opts.databaseTabs.map((d) => (
										<button key={d.key} className={`item${current?.kind === "ext" && current.key === d.key && section === "tables" ? " active" : ""}`} onClick={() => openExtTab(d.key)}>
											<span className="ic">{slotIcon(d.icon, 14, ICON.ext)}</span>
											<span className="name">{d.label}</span>
										</button>
									))}
								</div>
							)}
						/>
					)}
				</aside>

				<main className="main">
					{opts.banners.length > 0 && <div className="banners">{opts.banners.map((B, i) => <SlotBoundary key={i} name="Banner"><B ctx={slot} /></SlotBoundary>)}</div>}
					{section === "cloud" && <div className="cloudmain"><CloudMain cloud={cloud} page={cloudPage} setPage={setCloudPage} toast={toast} onOpen={openCloud} current={cloudTarget} slot={slot} /></div>}
					{dbName && section !== "cloud" && <WorkspaceTabs ws={ws} onPalette={() => setPalette(true)} extIcon={(t) => slotIcon(opts.databaseTabs.find((d) => d.key === t.key)?.icon, 13, ICON.ext)} />}
					{cloudTarget && cloud.auth.status === "out" && section !== "cloud" && (
						<div className="sessionbar" role="alert">
							<b>Signed out</b>
							<span title={cloud.auth.error ?? undefined}>{dbName} stops working when its access expires.</span>
							<span className="grow" />
							<button className="primary" onClick={signIn}>Sign in again</button>
						</div>
					)}
					<WorkspacePanes
						ws={ws}
						hidden={section === "cloud"}
						ask={cloudTarget}
						renderExt={(t) => <ExtTab tab={opts.databaseTabs.find((d) => d.key === t.key)} slot={slot} />}
						empty={dbName ? <WorkspaceEmpty ws={ws} onPalette={() => setPalette(true)} /> : (
										<>
											<Logo big />
											<h1>{branding.studioName}</h1>
											<p className="dim">A SQLite-compatible database that runs in your browser. Nothing is uploaded.</p>
											{recents.length + remoteList.length > 0 && (
												<div className="recents">
													<h4>Continue where you left off</h4>
													{remoteList.slice(0, 4).map((r) => (
														<div key={`remote:${r.name}`} className="recent">
															<button className="recent-open" onClick={() => openSaved(r.name)}>
																<span className="mono"><Globe size={12} /> {r.name}</span>
																<span className="dim small">{hostOf(r.url)}</span>
															</button>
															<button className="ghost icon" title="Forget this connection" onClick={() => { if (confirm(`Forget the connection ${r.name}? The database itself is not touched.`)) { forgetRemote(r.name); setRemoteList(remotes()); } }}>×</button>
														</div>
													))}
													{recents.slice(0, 6).map((r) => (
														<div key={r.name} className="recent">
															<button className="recent-open" onClick={() => openSaved(r.name)}>
																<span className="mono">{r.name}</span>
																<span className="dim small">{fmt(Math.ceil(r.size / 1024))} KB · {ago(r.at)}</span>
															</button>
															<button className="ghost icon" title="Forget it in this browser" onClick={async () => { if (confirm(`Forget ${r.name}? It is deleted from this browser.`)) { await forgetDb(r.name); setRecents(await listDbs()); } }}>×</button>
														</div>
													))}
												</div>
											)}
											<div className="starts">
												<button className="start" onClick={() => openDatabase("sample.musq", undefined, SAMPLE)}>
													<span className="si">★</span><b>Try the sample</b><span className="dim small">a small music database</span>
												</button>
												<button className="start" onClick={pickFile}>
													<span className="si">⇧</span><b>Open a file</b><span className="dim small">.sqlite, .db, .musq, or drop one</span>
												</button>
												<button className="start" onClick={() => openDatabase("untitled.musq")}>
													<span className="si">＋</span><b>Start empty</b><span className="dim small">a new database</span>
												</button>
												<button className="start" onClick={() => setConnecting({})}>
													<span className="si"><Globe size={18} /></span><b>Connect to a remote database…</b><span className="dim small">a libSQL or Hrana URL and a token</span>
												</button>
											</div>
										</>
						)}
					/>
				</main>

				<WorkspaceDialogs ws={ws} />
				{connecting && <ConnectDrawer initial={connecting} taken={(n) => n !== connecting.name && (!!remoteNamed(n) || recents.some((r) => r.name === n))} onConnect={connectRemote} onClose={() => setConnecting(null)} />}
				{palette && <Palette commands={commands} onClose={() => setPalette(false)} />}
				{keys && <Shortcuts onClose={() => setKeys(false)} />}
				{ws.ctxMenu}
				{dragging && (
					<div className="drop" onDragLeave={(e) => e.currentTarget === e.target && setDragging(false)}>
						<div>Drop a .sqlite, .musq, CSV or JSON file</div>
					</div>
				)}
			</div>
		</Slot.Provider></Studio.Provider>
	);
}

function ExtTab({ tab, slot }) {
	if (!tab || !slot.database) return null;
	const C = tab.component;
	return <div className="extpane"><div className="cloudpage"><SlotBoundary name={tab.label}><C ctx={slot} /></SlotBoundary></div></div>;
}
