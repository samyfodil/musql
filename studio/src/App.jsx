import { useCallback, useEffect, useMemo, useState } from "react";
import { call, ident, query, ready, rows } from "./db.js";
import { fmt } from "./values.js";
import { Studio } from "./studio.js";
import TableView from "./components/TableView.jsx";
import QueryView from "./components/QueryView.jsx";
import { SAMPLE } from "./sample.js";

const ICON = { table: "▦", view: "◇", index: "⋮", trigger: "ϟ", query: "›_" };
const GROUP = { table: "Tables", view: "Views", index: "Indexes", trigger: "Triggers" };

function useDark() {
	const mq = useMemo(() => matchMedia("(prefers-color-scheme: dark)"), []);
	const [dark, setDark] = useState(mq.matches);
	useEffect(() => {
		const on = (e) => setDark(e.matches);
		mq.addEventListener("change", on);
		return () => mq.removeEventListener("change", on);
	}, [mq]);
	return dark;
}

let tabSeq = 0;
let querySeq = 0;

export default function App() {
	const dark = useDark();
	const [dbName, setDbName] = useState(null);
	const [dirty, setDirty] = useState(false);
	const [schema, setSchema] = useState([]);
	const [columns, setColumns] = useState({});
	const [counts, setCounts] = useState({});
	const [tabs, setTabs] = useState([]);
	const [active, setActive] = useState(null);
	const [filter, setFilter] = useState("");
	const [msg, setMsg] = useState({ text: "Starting…" });
	const [dragging, setDragging] = useState(false);
	const [menu, setMenu] = useState(false);

	const status = useCallback((text, err) => setMsg({ text, err }), []);

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
		ready.then(() => status("Ready. Open a file, drop one here, or try the sample."), (e) => status(e.message, true));
	}, [status]);

	useEffect(() => {
		const warn = (e) => { if (dirty) e.preventDefault(); };
		addEventListener("beforeunload", warn);
		return () => removeEventListener("beforeunload", warn);
	}, [dirty]);

	// ---- tabs ----

	function openTab(tab) {
		setTabs((ts) => {
			const have = tab.table && ts.find((t) => t.table === tab.table);
			if (have) {
				setActive(have.id);
				return ts.map((t) => (t === have ? { ...t, mode: tab.mode ?? t.mode } : t));
			}
			const t = { id: ++tabSeq, ...tab };
			setActive(t.id);
			return [...ts, t];
		});
	}
	const openTable = (name, mode = "data") => openTab({ kind: "table", table: name, title: name, mode });
	const openQuery = (text = "") => openTab({ kind: "query", title: `Query ${++querySeq}`, text });

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
		if (file) await openDatabase(file.name, new Uint8Array(await file.arrayBuffer()));
	}

	async function exportAs(kind) {
		setMenu(false);
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
		const close = () => setMenu(false);
		const key = (e) => {
			if (e.key.toLowerCase() === "o" && (e.metaKey || e.ctrlKey)) {
				e.preventDefault();
				document.getElementById("file").click();
			}
		};
		addEventListener("click", close);
		addEventListener("keydown", key);
		return () => {
			removeEventListener("click", close);
			removeEventListener("keydown", key);
		};
	}, []);

	// Tabs on objects that no longer exist close themselves.
	useEffect(() => {
		setTabs((ts) => ts.filter((t) => t.kind !== "table" || schema.some((o) => o.name === t.table)));
	}, [schema]);

	const ctx = { schema, columnsOf, status, changed, dark };
	const activeTab = tabs.find((t) => t.id === active);
	const f = filter.trim().toLowerCase();

	return (
		<Studio.Provider value={ctx}>
			<div
				className="shell"
				onDragEnter={(e) => e.dataTransfer.types.includes("Files") && setDragging(true)}
				onDragOver={(e) => e.preventDefault()}
				onDrop={(e) => {
					e.preventDefault();
					setDragging(false);
					openFile(e.dataTransfer.files[0]);
				}}
			>
				<header className="bar">
					<div className="brand"><span className="logo">μ</span>musql <span className="dim">studio</span></div>
					<div className="dbname">{dbName ?? "no database"}{dirty && <span className="dot" title="Changed since it was opened or exported" />}</div>
					<div className="actions">
						<button title="A new, empty database" onClick={() => openDatabase("untitled.musq")}>New</button>
						<button title="A small music database to try things on" onClick={() => openDatabase("sample.musq", undefined, SAMPLE)}>Sample</button>
						<label className="button" title="Open a .musq or C SQLite file (⌘O), or drop one anywhere">
							Open…
							<input id="file" type="file" hidden accept=".musq,.sqlite,.sqlite3,.db" onChange={(e) => { openFile(e.target.files[0]); e.target.value = ""; }} />
						</label>
						<div className="menu" onClick={(e) => e.stopPropagation()}>
							<button className="primary" onClick={() => setMenu(!menu)}>Export ▾</button>
							{menu && (
								<div className="menu-list">
									<button onClick={() => exportAs("sqlite")}>SQLite file <span className="dim">.sqlite</span></button>
									<button onClick={() => exportAs("musql")}>musql file <span className="dim">.musq</span></button>
								</div>
							)}
						</div>
					</div>
				</header>

				<div className="layout">
					<aside className="side">
						<input className="filter" type="search" placeholder="Filter tables…" spellCheck={false} value={filter} onChange={(e) => setFilter(e.target.value)} />
						<button className="newquery" disabled={!dbName} onClick={() => openQuery()}>＋ New query</button>
						<nav>
							{["table", "view", "index", "trigger"].map((type) => {
								const items = schema.filter((o) => o.type === type && (!f || o.name.toLowerCase().includes(f)));
								if (!items.length) return null;
								return (
									<div className="group" key={type}>
										<h3>{GROUP[type]}<span>{items.length}</span></h3>
										{items.map((o) => {
											const own = type === "table" || type === "view";
											return (
												<button
													key={o.name}
													className={`item${activeTab?.table === o.name ? " active" : ""}`}
													title={o.sql ?? o.name}
													onClick={() => openTable(own ? o.name : o.tbl_name, own ? "data" : "structure")}
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
							{dbName && !schema.length && <p className="dim small pad">No tables yet. Write some SQL in a query tab.</p>}
						</nav>
					</aside>

					<main className="main">
						<div className="tabs">
							{tabs.map((t) => (
								<div key={t.id} className={`tab${t.id === active ? " active" : ""}`} onClick={() => setActive(t.id)} onAuxClick={(e) => e.button === 1 && closeTab(t.id)}>
									<span className="ic">{t.kind === "query" ? ICON.query : ICON[schema.find((o) => o.name === t.table)?.type ?? "table"]}</span>
									{t.title}
									<button className="x" title="Close" onClick={(e) => { e.stopPropagation(); closeTab(t.id); }}>×</button>
								</div>
							))}
						</div>
						<div className="panes">
							{tabs.map((t) => (
								<div key={t.id} className="pane" hidden={t.id !== active}>
									{t.kind === "table" ? <TableView name={t.table} mode={t.mode} /> : <QueryView initial={t.text} />}
								</div>
							))}
							{!tabs.length && (
								<div className="empty">
									<div className="logo big">μ</div>
									<h1>musql studio</h1>
									<p>A SQLite-compatible database in your browser, compiled to WebAssembly with its query JIT.</p>
									<p className="dim">Drop a <b>.sqlite</b> or <b>.musq</b> file anywhere, open one, or start from the sample.</p>
									<div className="cta">
										<button className="primary" onClick={() => openDatabase("sample.musq", undefined, SAMPLE)}>Try the sample</button>
										<button onClick={() => document.getElementById("file").click()}>Open a file…</button>
									</div>
								</div>
							)}
						</div>
					</main>
				</div>

				<footer className="status">
					<span className={msg.err ? "err" : ""}>{msg.text}</span>
					<span className="dim">musql · WebAssembly + JIT, in a worker</span>
				</footer>

				{dragging && (
					<div className="drop" onDragLeave={(e) => e.currentTarget === e.target && setDragging(false)}>
						<div>Drop a .sqlite or .musq file to open it</div>
					</div>
				)}
			</div>
		</Studio.Provider>
	);
}
