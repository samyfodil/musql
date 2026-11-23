// What a database open in the editor holds, wherever it came from: its
// schema, row counts, open tabs, the section in view and the dialogs. The
// studio (App.jsx) and DatabaseWorkspace both keep one.
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ident, query, rows } from "../db.js";
import { saved } from "../library.js";
import { isStudioTable } from "../vectors.js";
import { viewState } from "../view.js";
import { fmt } from "../values.js";
import { useContextMenu } from "../components/Menu.jsx";

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

export const ICON = { table: "▦", view: "◇", index: "⋮", trigger: "ϟ", query: "›_", diagram: "⌗", ext: "◆" };

/**
 * useWorkspace keeps one open database's view. status(text, isError) reports
 * to the page; onChanged runs after every change the editor makes; with
 * cloudSection a kept view may reopen on the studio's cloud section.
 */
export function useWorkspace({ status, dark, onChanged, initialSection = "tables", cloudSection = false }) {
	const [name, setName] = useState(null);
	const [schema, setSchema] = useState([]);
	const [columns, setColumns] = useState({});
	const [counts, setCounts] = useState({});
	const [tabs, setTabs] = useState([]);
	const [active, setActive] = useState(null);
	const [filter, setFilter] = useState("");
	const [section, setSection] = useState(initialSection); // "tables" | "sql" | "diagram", and the studio's "cloud"
	const [showInternal, setShowInternal] = useState(false);
	const [library, setLibrary] = useState(saved);
	const [dialog, setDialog] = useState(null); // "table" | {edit: table} | "import" | {import: file} | null
	const [epoch, setEpoch] = useState(0); // bumped when the whole database is replaced (undo, redo)
	const [ctxMenu, openCtx] = useContextMenu();
	const querySeq = useRef(0);
	const statusRef = useRef(status);
	statusRef.current = status;
	const changedRef = useRef(onChanged);
	changedRef.current = onChanged;
	const say = useCallback((text, err, action) => statusRef.current(text, err, action), []);

	useEffect(() => {
		const on = () => setLibrary(saved());
		addEventListener("musql-library", on);
		return () => removeEventListener("musql-library", on);
	}, []);

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
		changedRef.current?.();
		loadSchema().catch((e) => say(e.message, true));
	}, [loadSchema, say]);

	const columnsOf = useCallback((n) => columns[n] ?? [], [columns]);

	// ---- tabs ----

	function openTab(tab) {
		setTabs((ts) => {
			const have = (tab.table && ts.find((t) => t.table === tab.table)) || (tab.kind === "diagram" && ts.find((t) => t.kind === "diagram")) || (tab.kind === "ext" && ts.find((t) => t.kind === "ext" && t.key === tab.key));
			if (have) {
				setActive(have.id);
				return ts.map((t) => (t === have ? { ...t, mode: tab.mode ?? t.mode } : t));
			}
			const t = { id: crypto.randomUUID(), ...tab };
			setActive(t.id);
			return [...ts, t];
		});
	}
	const openTable = (n, mode = "data", state) => openTab({ kind: "table", table: n, title: n, mode, ...(state ? { state } : {}) });
	// tabState keeps what a tab reports about itself (a table's filter, sort and
	// view), for the URL and for reopening.
	const tabState = useCallback((id, state) => setTabs((ts) => ts.map((t) => (t.id === id && JSON.stringify(t.state) !== JSON.stringify(state) ? { ...t, state } : t))), []);
	const openQuery = (text = "", title = `Query ${++querySeq.current}`) => openTab({ kind: "query", title, text });
	const openDiagram = () => openTab({ kind: "diagram", title: "Diagram" });
	const retitle = (id, title) => setTabs((ts) => ts.map((t) => (t.id === id ? { ...t, title } : t)));
	const goTable = (n, mode) => {
		setSection("tables");
		openTable(n, mode);
	};

	function closeTab(id) {
		setTabs((ts) => {
			const i = ts.findIndex((t) => t.id === id);
			const next = ts.filter((t) => t.id !== id);
			if (active === id) setActive(next[Math.min(i, next.length - 1)]?.id ?? null);
			return next;
		});
	}

	/** show makes n, just opened with the schema s, the database in view, with the tabs view kept; it returns how many it restored. */
	function show(n, s, view) {
		const known = (t) => t.kind !== "table" || s.some((o) => o.name === t.table);
		const restored = (view?.tabs ?? []).filter(known).map((t) => ({ ...t, id: crypto.randomUUID() }));
		setTabs(restored);
		querySeq.current = Math.max(0, ...restored.map((t) => Number(/^Query (\d+)$/.exec(t.title ?? "")?.[1] ?? 0)));
		setActive(restored[view?.active]?.id ?? restored.at(-1)?.id ?? null);
		if (view?.section && (cloudSection || view.section !== "cloud")) setSection(view.section);
		setName(n);
		return restored.length;
	}

	/** openFirst opens the first table, or a query when there is none. */
	function openFirst(s) {
		const first = s.find((o) => o.type === "table");
		if (first) openTable(first.name);
		else openQuery();
	}

	// enter shows a section, opening the diagram, or a query when none is open.
	function enter(sec) {
		setSection(sec);
		if (sec === "diagram" && name) openDiagram();
		if (sec === "sql" && name && !tabs.some((t) => t.kind === "query")) openQuery();
	}

	// The open tabs are kept per database.
	useEffect(() => {
		if (!name) return;
		// An extension's tab belongs to the session, not to the database's kept view.
		const kept = tabs.filter((t) => t.kind !== "ext");
		viewState(name, { tabs: kept.map(({ id, ...t }) => t), active: kept.findIndex((t) => t.id === active), section });
	}, [tabs, active, section, name]);

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
				say(`Dropped ${o.name}.`);
			} catch (e) {
				say(e.message, true);
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

	const internal = useMemo(() => internalNames(schema), [schema]);

	/** commands is what the palette finds of this database: objects, saved queries, then its actions. */
	function commands() {
		const out = [];
		for (const o of schema) {
			if ((o.type !== "table" && o.type !== "view") || internal.has(o.name)) continue;
			out.push({ group: o.type === "table" ? "Tables" : "Views", icon: ICON[o.type], label: o.name, hint: counts[o.name] !== undefined ? `${fmt(counts[o.name])} rows` : "", run: () => goTable(o.name) });
		}
		for (const q of library) out.push({ group: "Saved queries", icon: ICON.query, label: q.name, run: () => { setSection("sql"); openQuery(q.sql, q.name); } });
		if (name) {
			const act = (label, run, hint, icon) => out.push({ group: "Actions", label, run, hint, icon });
			act("New query", () => { setSection("sql"); openQuery(); }, "SQL editor", ICON.query);
			act("Create a table", () => setDialog("table"), "", "＋");
			act("Import CSV or JSON", () => setDialog("import"), "", "⇪");
			act("Schema diagram", () => { setSection("diagram"); openDiagram(); }, "", ICON.diagram);
		}
		return out;
	}

	const tabsShown = tabs.filter((t) => (section === "sql" ? t.kind === "query" : section === "diagram" ? t.kind === "diagram" : t.kind === "table" || t.kind === "ext"));
	const current = tabsShown.find((t) => t.id === active) ?? tabsShown.at(-1);

	// What every view shares (studio.js).
	const ctx = { schema, columnsOf, status: say, changed, dark, openQuery, epoch, tabState, editTable: (n) => setDialog({ edit: n }), importInto: () => setDialog("import") };

	return {
		name, schema, counts, tabs, setTabs, active, setActive, filter, setFilter, section, setSection, showInternal, setShowInternal, library, dialog, setDialog, epoch, setEpoch,
		internal, tabsShown, current, ctx, ctxMenu, openCtx, status: say, loadSchema, changed,
		openTab, openTable, openQuery, openDiagram, goTable, retitle, closeTab, show, openFirst, enter, objectMenu, commands,
	};
}
