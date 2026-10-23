import { Suspense, lazy, useCallback, useEffect, useMemo, useState } from "react";
import { call, ident, query, ready, rows } from "./db.js";
import { fmt } from "./values.js";
import { Studio } from "./studio.js";
import TableView from "./components/TableView.jsx";
import { BusyBar, Spinner } from "./components/Busy.jsx";
import { MenuButton, useContextMenu } from "./components/Menu.jsx";
import { useToasts } from "./components/Toasts.jsx";
import Palette from "./components/Palette.jsx";
import { Contrast, Moon, Network, Search, SquareTerminal, Sun, Table2 } from "lucide-react";
import { ImportDialog, TableDialog } from "./components/Dialogs.jsx";
import { SAMPLE } from "./sample.js";
import { saved, unsave } from "./library.js";
import { isStudioTable } from "./vectors.js";

// The editor (CodeMirror) loads with the first query tab, not with the page.
const QueryView = lazy(() => import("./components/QueryView.jsx"));
const Diagram = lazy(() => import("./components/Diagram.jsx"));

// The tables a virtual table keeps its data in, by module: listed only on
// request, as the sqlite3 shell's .tables leaves them out too.
const SHADOW = {
	fts5: ["data", "idx", "content", "docsize", "config"],
	fts4: ["content", "segments", "segdir", "docsize", "stat"],
	fts3: ["content", "segments", "segdir", "docsize", "stat"],
	rtree: ["node", "parent", "rowid"],
	rtree_i32: ["node", "parent", "rowid"],
	vec1: ["config", "base", "idx", "model", "meta", "idx_idx"],
};

/** The names of schema's internal objects: shadow tables, their indexes, and the studio's own. */
function internalNames(schema) {
	const out = new Set();
	for (const o of schema) {
		const m = /^\s*CREATE\s+VIRTUAL\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?\S+\s+USING\s+(\w+)/i.exec(o.sql ?? "");
		for (const sfx of SHADOW[m?.[1]?.toLowerCase()] ?? []) out.add(`${o.name}_${sfx}`);
		if (isStudioTable(o.name) || /^musql_studio_/.test(o.name)) out.add(o.name);
	}
	for (const o of schema) if (out.has(o.tbl_name) && o.type !== "table") out.add(o.name);
	return out;
}

const ICON = { table: "▦", view: "◇", index: "⋮", trigger: "ϟ", query: "›_", diagram: "⌗" };
const GROUP = { table: "Tables", view: "Views", index: "Indexes", trigger: "Triggers" };

const THEMES = { system: <Contrast size={17} />, light: <Sun size={17} />, dark: <Moon size={17} /> };

// useTheme is the chosen theme (kept in this browser) and whether the page is
// dark: the system's choice unless one is picked.
function useTheme() {
	const mq = useMemo(() => matchMedia("(prefers-color-scheme: dark)"), []);
	const [systemDark, setSystemDark] = useState(mq.matches);
	const [theme, setTheme] = useState(() => {
		try {
			return localStorage.getItem("musql-theme") ?? "system";
		} catch {
			return "system";
		}
	});
	useEffect(() => {
		const on = (e) => setSystemDark(e.matches);
		mq.addEventListener("change", on);
		return () => mq.removeEventListener("change", on);
	}, [mq]);
	useEffect(() => {
		if (theme === "system") delete document.documentElement.dataset.theme;
		else document.documentElement.dataset.theme = theme;
		try {
			localStorage.setItem("musql-theme", theme);
		} catch {
			// a private window keeps the choice for this page only
		}
	}, [theme]);
	return [theme, setTheme, theme === "dark" || (theme === "system" && systemDark)];
}

let querySeq = 0;

export default function App() {
	const [theme, setTheme, dark] = useTheme();
	const [dbName, setDbName] = useState(null);
	const [dirty, setDirty] = useState(false);
	const [schema, setSchema] = useState([]);
	const [columns, setColumns] = useState({});
	const [counts, setCounts] = useState({});
	const [tabs, setTabs] = useState([]);
	const [active, setActive] = useState(null);
	const [filter, setFilter] = useState("");
	const [dragging, setDragging] = useState(false);
	const [section, setSection] = useState("tables"); // the rail: "tables" | "sql" | "diagram"
	const [palette, setPalette] = useState(false);
	const [showInternal, setShowInternal] = useState(false);
	const [library, setLibrary] = useState(saved);
	const [dialog, setDialog] = useState(null); // "table" | {edit: table} | "import" | {import: file} | null
	const [sideOpen, setSideOpen] = useState(false); // the sidebar, on a narrow screen
	const [ctxMenu, openCtx] = useContextMenu();

	useEffect(() => {
		const on = () => setLibrary(saved());
		addEventListener("musql-library", on);
		return () => removeEventListener("musql-library", on);
	}, []);

	const [toasts, toast] = useToasts();
	const status = useCallback((text, err) => { if (err || !/^Opening /.test(text)) toast(text, err); }, [toast]);

	// loadSchema reads the catalog, every table's columns and row count.
	const loadSchema = useCallback(async () => {
		const s = await rows("SELECT type, name, tbl_name, sql FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' ORDER BY name COLLATE NOCASE");
		const cols = {};
		const n = {};
		for (const o of s) {
			if (o.type !== "table" && o.type !== "view") continue;
			try {
				cols[o.name] = (await rows(`PRAGMA table_info(${ident(o.name)})`)).map((c) => c.name);
				if (o.type === "table") n[o.name] = (await query(`SELECT count(*) FROM ${ident(o.name)}`)).rows[0][0];
			} catch {
				// a virtual table whose module is missing still lists, uncounted
			}
		}
		setSchema(s);
		setColumns(cols);
		setCounts(n);
		return s;
	}, []);

	const changed = useCallback(() => {
		setDirty(true);
		loadSchema().catch((e) => status(e.message, true));
	}, [loadSchema, status]);

	const columnsOf = useCallback((name) => columns[name] ?? [], [columns]);

	useEffect(() => {
		ready.catch((e) => status(e.message, true));
	}, [status]);

	useEffect(() => {
		const warn = (e) => { if (dirty) e.preventDefault(); };
		addEventListener("beforeunload", warn);
		return () => removeEventListener("beforeunload", warn);
	}, [dirty]);

	// ---- tabs ----

	function openTab(tab) {
		setTabs((ts) => {
			const have = (tab.table && ts.find((t) => t.table === tab.table)) || (tab.kind === "diagram" && ts.find((t) => t.kind === "diagram"));
			if (have) {
				setActive(have.id);
				return ts.map((t) => (t === have ? { ...t, mode: tab.mode ?? t.mode } : t));
			}
			const t = { id: crypto.randomUUID(), ...tab };
			setActive(t.id);
			return [...ts, t];
		});
	}
	const openTable = (name, mode = "data") => openTab({ kind: "table", table: name, title: name, mode });
	const openQuery = (text = "", title = `Query ${++querySeq}`) => openTab({ kind: "query", title, text });
	const openDiagram = () => openTab({ kind: "diagram", title: "Diagram" });
	const retitle = (id, title) => setTabs((ts) => ts.map((t) => (t.id === id ? { ...t, title } : t)));

	function closeTab(id) {
		setTabs((ts) => {
			const i = ts.findIndex((t) => t.id === id);
			const next = ts.filter((t) => t.id !== id);
			if (active === id) setActive(next[Math.min(i, next.length - 1)]?.id ?? null);
			return next;
		});
	}

	// ---- databases ----

	async function openDatabase(name, data, then) {
		if (dirty && !confirm("Discard the changes to the open database? Export it first to keep them.")) return;
		status(`Opening ${name}…`);
		try {
			const t = performance.now();
			await call("open", data);
			if (then) await query(then);
			setDbName(name);
			setDirty(false);
			setTabs([]);
			querySeq = 0;
			const s = await loadSchema();
			status(`Opened ${name} in ${(performance.now() - t).toFixed(0)} ms`);
			const first = s.find((o) => o.type === "table");
			if (first) openTable(first.name);
			else openQuery();
		} catch (e) {
			status(`Could not open ${name}: ${e.message}`, true);
		}
	}

	async function openFile(file) {
		if (!file) return;
		if (/\.(csv|tsv|json)$/i.test(file.name)) {
			if (!dbName) await openDatabase("untitled.musq");
			setDialog({ import: file });
			return;
		}
		await openDatabase(file.name, new Uint8Array(await file.arrayBuffer()));
	}

	async function exportAs(kind) {
		if (!dbName) return status("Nothing to export yet.", true);
		try {
			const bytes = await call(kind === "sqlite" ? "exportSQLite" : "exportMusql");
			const a = document.createElement("a");
			a.href = URL.createObjectURL(new Blob([bytes]));
			a.download = `${dbName.replace(/\.(musq|sqlite3?|db)$/i, "")}.${kind === "sqlite" ? "sqlite" : "musq"}`;
			a.click();
			setTimeout(() => URL.revokeObjectURL(a.href), 1000);
			setDirty(false);
			status(`Exported ${a.download} · ${fmt(Math.ceil(bytes.length / 1024))} KB`);
		} catch (e) {
			status(`Export failed: ${e.message}`, true);
		}
	}

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

	// Tabs on objects that no longer exist close themselves.
	useEffect(() => {
		setTabs((ts) => ts.filter((t) => t.kind !== "table" || schema.some((o) => o.name === t.table)));
	}, [schema]);

	// objectMenu is a right-click on a sidebar entry.
	function objectMenu(o) {
		const own = o.type === "table" || o.type === "view";
		const drop = async () => {
			if (!confirm(`Drop ${o.type} ${o.name}? This cannot be undone.`)) return;
			try {
				await query(`DROP ${o.type.toUpperCase()} ${ident(o.name)}`);
				changed();
				status(`Dropped ${o.name}.`);
			} catch (e) {
				status(e.message, true);
			}
		};
		return [
			own && { label: "Open", onClick: () => openTable(o.name, "data") },
			{ label: "Structure", onClick: () => openTable(own ? o.name : o.tbl_name, "structure") },
			o.type === "table" && { label: "Edit table…", onClick: () => setDialog({ edit: o.name }) },
			own && { label: "Query it", onClick: () => openQuery(`SELECT * FROM ${ident(o.name)}\nLIMIT 100;`, o.name) },
			{ label: "Copy name", onClick: () => navigator.clipboard?.writeText(o.name) },
			{ label: "Copy definition", disabled: !o.sql, onClick: () => navigator.clipboard?.writeText(`${o.sql};`) },
			"-",
			{ label: `Drop ${o.type}`, danger: true, onClick: drop },
		];
	}

	const ctx = { schema, columnsOf, status, changed, dark, openQuery, editTable: (name) => setDialog({ edit: name }), importInto: () => setDialog("import") };
	const activeTab = tabs.find((t) => t.id === active);
	const f = filter.trim().toLowerCase();
	const internal = useMemo(() => internalNames(schema), [schema]);
	const hidden = showInternal ? 0 : schema.filter((o) => internal.has(o.name)).length;
	const pickFile = () => document.getElementById("file").click();
	const fileItems = [
		{ label: "New empty database", onClick: () => openDatabase("untitled.musq") },
		{ label: "Open a file…", hint: "⌘O", onClick: pickFile },
		{ label: "Open the sample", onClick: () => openDatabase("sample.musq", undefined, SAMPLE) },
		"-",
		{ label: "Export as SQLite", hint: ".sqlite", disabled: !dbName, onClick: () => exportAs("sqlite") },
		{ label: "Export as musql", hint: ".musq", disabled: !dbName, onClick: () => exportAs("musql") },
	];

	// Everything the palette finds: objects first, then actions.
	const commands = useMemo(() => {
		const out = [];
		for (const o of schema) {
			if ((o.type !== "table" && o.type !== "view") || internal.has(o.name)) continue;
			out.push({ group: o.type === "table" ? "Tables" : "Views", icon: ICON[o.type], label: o.name, hint: counts[o.name] !== undefined ? `${fmt(counts[o.name])} rows` : "", run: () => { setSection("tables"); openTable(o.name); } });
		}
		for (const q of library) out.push({ group: "Saved queries", icon: ICON.query, label: q.name, run: () => { setSection("sql"); openQuery(q.sql, q.name); } });
		const act = (label, run, hint, icon = "→") => out.push({ group: "Actions", label, run, hint, icon });
		if (dbName) {
			act("New query", () => { setSection("sql"); openQuery(); }, "SQL editor", ICON.query);
			act("Create a table", () => setDialog("table"), "", "＋");
			act("Import CSV or JSON", () => setDialog("import"), "", "⇪");
			act("Schema diagram", () => { setSection("diagram"); openDiagram(); }, "", ICON.diagram);
			act("Export as SQLite file", () => exportAs("sqlite"), ".sqlite", "⇩");
			act("Export as musql file", () => exportAs("musql"), ".musq", "⇩");
		}
		act("Open a file", pickFile, "⌘O", "⇧");
		act("Open the sample database", () => openDatabase("sample.musq", undefined, SAMPLE), "", "★");
		act("New empty database", () => openDatabase("untitled.musq"), "", "＋");
		act(`Theme: ${theme === "dark" ? "light" : "dark"}`, () => setTheme(dark ? "light" : "dark"), "", dark ? "☀" : "☾");
		return out;
	}, [schema, internal, counts, library, dbName, theme, dark]);

	const tabsShown = tabs.filter((t) => (section === "sql" ? t.kind === "query" : section === "diagram" ? t.kind === "diagram" : t.kind === "table"));
	const current = tabsShown.find((t) => t.id === active) ?? tabsShown.at(-1);

	function goSection(sec) {
		setSection(sec);
		// On a narrow screen the list is an overlay a rail button opens and closes.
		setSideOpen(matchMedia("(max-width: 760px)").matches && (sec !== section || !sideOpen) && sec !== "diagram");
		if (sec === "diagram" && dbName) openDiagram();
		if (sec === "sql" && dbName && !tabs.some((t) => t.kind === "query")) openQuery();
	}

	return (
		<Studio.Provider value={ctx}>
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
					<div className="logo" title="musql studio">μ</div>
					{[
						["tables", <Table2 size={18} />, "Table editor"],
						["sql", <SquareTerminal size={18} />, "SQL editor"],
						["diagram", <Network size={18} />, "Schema diagram"],
					].map(([k, ic, label]) => (
						<button key={k} className={`railbtn${section === k ? " on" : ""}`} title={label} disabled={!dbName && k !== "tables"} onClick={() => goSection(k)}>
							{ic}
						</button>
					))}
					<span className="grow" />
					<button className="railbtn" title="Search everything (⌘K)" onClick={() => setPalette(true)}><Search size={17} /></button>
					<button className="railbtn" title={`Theme: ${theme} (click to change)`} onClick={() => setTheme(theme === "system" ? "light" : theme === "light" ? "dark" : "system")}>{THEMES[theme]}</button>
				</nav>

				<aside className="side">
					<div className="dbswitch">
						<MenuButton
							className="dbbtn"
							label={<><span className="dbn">{dbName ?? "No database"}</span>{dirty && <span className="dot" title="Changed since it was opened or exported" />}<span className="dim">▾</span></>}
							items={fileItems}
						/>
					</div>
					{section === "sql" ? (
						<>
							<div className="sidehead">
								<h3>SQL editor</h3>
								<button className="ghost icon" title="New query" disabled={!dbName} onClick={() => openQuery()}>＋</button>
							</div>
							<nav>
								<div className="group">
									<h4>Open</h4>
									{tabs.filter((t) => t.kind === "query").map((t) => (
										<button key={t.id} className={`item${current?.id === t.id ? " active" : ""}`} onClick={() => setActive(t.id)}>
											<span className="ic">{ICON.query}</span><span className="name">{t.title}</span>
										</button>
									))}
								</div>
								{library.length > 0 && (
									<div className="group">
										<h4>Saved</h4>
										{library.map((q) => (
											<div key={q.name} className="item saved">
												<button className="name" title={q.sql} onClick={() => openQuery(q.sql, q.name)}>
													<span className="ic">★</span>{q.name}
												</button>
												<button className="x" title="Forget this saved query" onClick={() => confirm(`Forget the saved query “${q.name}”?`) && unsave(q.name)}>×</button>
											</div>
										))}
									</div>
								)}
							</nav>
						</>
					) : (
						<>
							<div className="sidehead">
								<h3>Tables</h3>
								<button className="ghost icon" title="Create a table" disabled={!dbName} onClick={() => setDialog("table")}>＋</button>
							</div>
							<input className="filter" type="search" placeholder="Search tables" spellCheck={false} value={filter} onChange={(e) => setFilter(e.target.value)} />
							<nav onClick={(e) => e.target.closest("button.item") && setSideOpen(false)}>
								{["table", "view", "index", "trigger"].map((type) => {
									const items = schema.filter((o) => o.type === type && (showInternal || !internal.has(o.name)) && (!f || o.name.toLowerCase().includes(f)));
									if (!items.length || ((type === "index" || type === "trigger") && !f && !showInternal)) return null;
									return (
										<div className="group" key={type}>
											{type !== "table" && <h4>{GROUP[type]}</h4>}
											{items.map((o) => {
												const own = type === "table" || type === "view";
												return (
													<button
														key={o.name}
														className={`item${current?.table === o.name && section === "tables" ? " active" : ""}`}
														title={o.sql ?? o.name}
														onClick={() => { setSection("tables"); openTable(own ? o.name : o.tbl_name, own ? "data" : "structure"); }}
														onContextMenu={(e) => openCtx(e, objectMenu(o))}
													>
														<span className="ic">{ICON[type]}</span>
														<span className="name">{o.name}</span>
														{counts[o.name] !== undefined && <span className="count">{fmt(counts[o.name])}</span>}
													</button>
												);
											})}
										</div>
									);
								})}
								{dbName && !schema.length && (
									<div className="sideempty">
										<p className="dim small">No tables yet.</p>
										<button className="primary" onClick={() => setDialog("table")}>Create a table</button>
										<button onClick={() => setDialog("import")}>Import a CSV</button>
									</div>
								)}
								{dbName && (hidden > 0 || showInternal || schema.some((o) => o.type === "index" || o.type === "trigger")) && (
									<button className="link small pad" onClick={() => setShowInternal(!showInternal)}>
										{showInternal ? "Show less" : "Show indexes, triggers and internal tables"}
									</button>
								)}
							</nav>
						</>
					)}
				</aside>

				<main className="main">
					{dbName && (
						<div className="tabs">
							{tabsShown.map((t) => (
								<div key={t.id} className={`tab${t.id === current?.id ? " active" : ""}`} onClick={() => setActive(t.id)} onAuxClick={(e) => e.button === 1 && closeTab(t.id)}>
									<span className="ic">{t.kind === "table" ? ICON[schema.find((o) => o.name === t.table)?.type ?? "table"] : ICON[t.kind]}</span>
									{t.title}
									<button className="x" title="Close" onClick={(e) => { e.stopPropagation(); closeTab(t.id); }}>×</button>
								</div>
							))}
							{section === "sql" && <button className="ghost icon newtab" title="New query" onClick={() => openQuery()}>＋</button>}
							<span className="grow" />
							<button className="kbdbtn" onClick={() => setPalette(true)}><Search size={13} /><span className="dim">Search</span> <span className="kbd">⌘K</span></button>
						</div>
					)}
					<div className="panes">
						{tabs.map((t) => (
							<div key={t.id} className="pane" hidden={t.id !== current?.id}>
								{t.kind === "table" ? (
									<TableView name={t.table} mode={t.mode} />
								) : t.kind === "diagram" ? (
									<Suspense fallback={<Spinner label="Loading the diagram…" />}>
										<Diagram onOpen={(name) => { setSection("tables"); openTable(name); }} />
									</Suspense>
								) : (
									<Suspense fallback={<Spinner label="Loading the editor…" />}>
										<QueryView initial={t.text} title={t.title} onTitle={(title) => retitle(t.id, title)} />
									</Suspense>
								)}
							</div>
						))}
						{!current && (
							<div className="empty">
								{dbName ? (
									<>
										<h1>{section === "sql" ? "Write some SQL" : "Pick a table"}</h1>
										<p className="dim">{section === "sql" ? "Open a new query, or a saved one from the list." : "Choose one on the left, or create one."}</p>
										<div className="cta">
											{section === "sql" ? <button className="primary" onClick={() => openQuery()}>New query</button> : <button className="primary" onClick={() => setDialog("table")}>Create a table</button>}
											<button onClick={() => setPalette(true)}>Search <span className="kbd">⌘K</span></button>
										</div>
									</>
								) : (
									<>
										<div className="logo big">μ</div>
										<h1>musql studio</h1>
										<p className="dim">A SQLite-compatible database that runs in your browser. Nothing is uploaded.</p>
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
										</div>
									</>
								)}
							</div>
						)}
					</div>
				</main>

				{(dialog === "table" || dialog?.edit) && (
					<TableDialog
						table={dialog?.edit}
						tables={schema.filter((o) => o.type === "table" && !internal.has(o.name)).map((o) => o.name)}
						status={status}
						onSQL={(sql) => { setSection("sql"); openQuery(sql, dialog?.edit ? `Edit ${dialog.edit}` : "New table"); }}
						onClose={(name) => {
							const was = dialog?.edit;
							setDialog(null);
							if (name) {
								changed();
								setSection("tables");
								openTable(name, "data");
								status(was ? `Saved ${name}.` : `Created ${name}.`);
							}
						}}
					/>
				)}
				{(dialog === "import" || dialog?.import) && (
					<ImportDialog file={dialog?.import} status={status} tables={schema.filter((o) => o.type === "table").map((o) => o.name)} onClose={(name) => {
						setDialog(null);
						if (name) {
							changed();
							setSection("tables");
							openTable(name);
						}
					}} />
				)}
				{palette && <Palette commands={commands} onClose={() => setPalette(false)} />}
				{ctxMenu}
				{toasts}
				{dragging && (
					<div className="drop" onDragLeave={(e) => e.currentTarget === e.target && setDragging(false)}>
						<div>Drop a .sqlite, .musq, CSV or JSON file</div>
					</div>
				)}
			</div>
		</Studio.Provider>
	);
}
