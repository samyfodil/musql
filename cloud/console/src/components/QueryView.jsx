import { useEffect, useMemo, useRef, useState } from "react";
import CodeMirror from "@uiw/react-codemirror";
import { EditorView, keymap } from "@codemirror/view";
import { Prec } from "@codemirror/state";
import { closeCompletion } from "@codemirror/autocomplete";
import { sql, SQLite } from "@codemirror/lang-sql";
import { lintGutter, setDiagnostics } from "@codemirror/lint";
import { format } from "sql-formatter";
import { History as HistoryIcon, ListChecks, PanelRight, WandSparkles } from "lucide-react";
import { query, run, split } from "../db.js";
import { download, fmt, toCSV, toJSON } from "../values.js";
import { useStudio } from "../studio.js";
import { DEFAULT_MODEL, embed, vectorBlob } from "../embed.js";
import { expandEmbeds, verb } from "../sqltext.js";
import { history, remember, save } from "../library.js";
import { functionHints } from "../fnhints.js";
import AskAI from "./AskAI.jsx";
import Grid from "./Grid.jsx";
import { MenuButton } from "./Menu.jsx";
import Chart from "./Chart.jsx";
import PlanFlow from "./PlanFlow.jsx";
import { Spinner } from "./Busy.jsx";

const READS = new Set(["SELECT", "WITH", "VALUES", "EXPLAIN", "PRAGMA"]);

/**
 * A SQL editor and its results. Ctrl/Cmd+Enter runs the selection or all of
 * it, a statement at a time, stopping at the first error. embed('text') binds
 * the text's vector (embed.js).
 */
export default function QueryView({ initial = "", title, onTitle, ask }) {
	const { schema, columnsOf, status, changed, dark } = useStudio();
	const [text, setText] = useState(initial);
	const [runs, setRuns] = useState([]); // newest first: {at, sql, results}
	const [runAt, setRunAt] = useState(0);
	const results = runs[runAt]?.results ?? [];
	const [shown, setShown] = useState(0);
	const [schemaOpen, setSchemaOpen] = useState(() => {
		try {
			return localStorage.getItem("musql-studio:schema-panel") !== "0";
		} catch {
			return true;
		}
	});
	const [plan, setPlan] = useState(null); // {sql, tree, code?}
	const [busy, setBusy] = useState(false);
	const [askOpen, setAskOpen] = useState(false);
	const [menu, setMenu] = useState(null); // "history" | null
	const [as, setAs] = useState("grid"); // how a result shows: "grid" | "chart"
	const [planAs, setPlanAs] = useState("flow"); // "flow" | "text"
	const view = useRef(null);
	const go = useRef(null);

	// target is what a run runs: the selection; else, unless all is asked for,
	// the statement the cursor is in (or just after); else the whole editor.
	// It comes with the offset it starts at, to mark an error there.
	async function target(all = false) {
		const st = view.current?.state;
		const doc = st ? st.doc.toString() : text;
		if (st && !st.selection.main.empty) {
			const { from, to } = st.selection.main;
			return { text: st.sliceDoc(from, to).trim(), at: from };
		}
		if (all || !st) return { text: doc.trim(), at: Math.max(0, doc.search(/\S/)) };
		const stmts = await split(doc).catch(() => []);
		const head = st.selection.main.head;
		let pos = 0;
		let pick = null;
		for (const s of stmts) {
			const at = doc.indexOf(s, pos);
			if (at < 0) break;
			if (pick && at > head) break;
			pick = { text: s, at };
			pos = at + s.length;
		}
		return pick ?? { text: doc.trim(), at: 0 };
	}

	// mark underlines a failed statement in the editor with its error; no message clears.
	function mark(from, to, message) {
		const v = view.current;
		if (!v) return;
		const len = v.state.doc.length;
		v.dispatch(setDiagnostics(v.state, message ? [{ from: Math.min(from, len), to: Math.min(Math.max(to, from + 1), len), severity: "error", message }] : []));
	}

	function tidy() {
		const v = view.current;
		if (!v) return;
		try {
			const out = format(v.state.doc.toString(), { language: "sqlite", keywordCase: "upper", tabWidth: 2 });
			v.dispatch({ changes: { from: 0, to: v.state.doc.length, insert: out } });
		} catch (e) {
			status(`Could not format: ${e.message}`, true);
		}
	}

	function insert(t) {
		const v = view.current;
		if (!v) return;
		const { from, to } = v.state.selection.main;
		v.dispatch({ changes: { from, to, insert: t }, selection: { anchor: from + t.length } });
		v.focus();
	}

	// bind embeds every embed('…') of a statement: the statement with ? in their
	// place, and the vectors for them.
	async function bind(stmt) {
		const { sql, calls } = expandEmbeds(stmt);
		if (!calls.length) return { sql, params: [] };
		const params = [];
		for (const c of calls) {
			const [v] = await embed([c.text], c.model ?? DEFAULT_MODEL, "query");
			params.push(vectorBlob(v));
		}
		return { sql, params };
	}

	go.current = async (all = false) => {
		const { text: q, at: qAt } = await target(all);
		if (!q) return;
		mark(0, 0, null);
		setBusy(true);
		setPlan(null);
		const out = [];
		let wrote = false;
		const t0 = performance.now();
		try {
			const stmts = await split(q);
			remember(q);
			let from = 0;
			for (const s of stmts) {
				const t = performance.now();
				const sAt = Math.max(0, q.indexOf(s, from));
				from = sAt + s.length;
				try {
					const { sql, params } = await bind(s);
					if (READS.has(verb(sql))) {
						const r = await query(sql, params);
						out.push({ sql: s, ...r, verb: verb(sql), meta: `${fmt(r.rows.length)} row${r.rows.length === 1 ? "" : "s"} · ${r.ms.toFixed(2)} ms` });
						if (verb(sql) === "PRAGMA") wrote = true; // a PRAGMA can set as well as read
					} else {
						const r = await run(sql, params);
						wrote = true;
						// Only INSERT, UPDATE, DELETE and REPLACE count rows: after any other
						// statement changes() still holds the last count before it.
						const counted = /^(INSERT|UPDATE|DELETE|REPLACE)$/.test(verb(sql));
						const ms = performance.now() - t;
						out.push({ sql: s, columns: [], rows: [], verb: verb(sql), ms, changes: counted ? r.changes : undefined, meta: counted ? `${fmt(r.changes)} row${r.changes === 1 ? "" : "s"} changed · ${ms.toFixed(2)} ms` : `Done · ${ms.toFixed(2)} ms` });
					}
				} catch (e) {
					out.push({ sql: s, verb: verb(s), error: e.message, ms: performance.now() - t });
					mark(qAt + sAt, qAt + sAt + s.length, e.message);
					break;
				}
			}
		} catch (e) {
			out.push({ sql: q, verb: verb(q), error: e.message });
		} finally {
			setBusy(false);
		}
		setRuns((rs) => [{ at: Date.now(), sql: q, results: out }, ...rs].slice(0, 20));
		setRunAt(0);
		setShown(defaultShown(out));
		const failed = out.find((r) => r.error);
		const ms = (performance.now() - t0).toFixed(1);
		if (failed) status(out.length > 1 ? `Statement ${out.length} of the script failed: ${failed.error}` : failed.error, true);
		if (wrote) changed();
	};

	async function explain(code) {
		const s = (await target()).text;
		if (!s) return;
		try {
			const { sql, params } = await bind(s);
			const qp = await query(`EXPLAIN QUERY PLAN ${sql}`, params);
			const bc = code ? await query(`EXPLAIN ${sql}`, params) : null;
			setPlan({ sql: s, rows: qp.rows, tree: planTree(qp.rows), code: bc });
		} catch (e) {
			setPlan({ sql: s, error: e.message });
		}
	}

	async function saveAs() {
		const q = (await target(true)).text;
		if (!q) return;
		const name = prompt("Save this query as:", title?.startsWith("Query ") ? "" : title);
		if (!name) return;
		save(name, q);
		onTitle?.(name);
		status(`Saved “${name}”.`);
	}

	// asView makes the SELECT in the editor (or its selection) a view.
	async function asView() {
		const q = (await target()).text.replace(/;\s*$/, "");
		if (!q) return;
		if (!["SELECT", "WITH", "VALUES"].includes(verb(q))) return status("Only a SELECT can become a view.", true);
		const name = prompt("Create a view named:");
		if (!name) return;
		try {
			await run(`CREATE VIEW "${name.replace(/"/g, '""')}" AS ${q}`);
			changed();
			status(`Created the view ${name}.`);
		} catch (e) {
			status(e.message, true);
		}
	}

	// Completion knows every table and view and their columns, and the functions
	// with their signatures (fnhints.js). Ctrl/Cmd+Enter is
	// taken before any keymap, so an open completion list cannot swallow it.
	const extensions = useMemo(() => {
		const tables = {};
		for (const o of schema) if (o.type === "table" || o.type === "view") tables[o.name] = columnsOf(o.name);
		const runKey = (all) => (v) => {
			closeCompletion(v);
			go.current(all);
			return true;
		};
		return [
			sql({ dialect: SQLite, schema: tables, upperCaseKeywords: true }),
			functionHints,
			// Declared up front so a run's error marks survive the editor reconfiguring.
			lintGutter(),
			EditorView.lineWrapping,
			EditorView.domEventHandlers({
				keydown(e, v) {
					if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
						e.preventDefault();
						return runKey(e.shiftKey)(v);
					}
					return false;
				},
			}),
			Prec.highest(keymap.of([
				{ key: "Mod-Enter", run: runKey(false) },
				{ key: "Shift-Mod-Enter", run: runKey(true) },
				{ key: "Shift-Alt-f", run: () => (tidy(), true) },
			])),
		];
	}, [schema, columnsOf]);

	useEffect(() => {
		const close = () => setMenu(null);
		addEventListener("click", close);
		return () => removeEventListener("click", close);
	}, []);

	const r = shown === "summary" ? null : results[shown];
	const hist = menu === "history" ? history() : [];

	return (
		<>
			<div className="toolbar">
				<button className="primary" disabled={busy} title="Run the selection, or the statement at the cursor (⌘Enter)" onClick={() => go.current()}>{busy ? <><span className="spinner inline" /> Running</> : "Run"}</button>
				<button className="ghost" disabled={busy} title="Run every statement (⇧⌘Enter)" onClick={() => go.current(true)}>Run all</button>
				<button className="ghost icon" title="Format the SQL (⇧⌥F)" onClick={tidy}><WandSparkles size={15} /></button>
				{ask && <button title="Describe what you want; the SQL is put in the editor, never run" onClick={() => setAskOpen((o) => !o)}>Ask AI</button>}
				<button title="How the database runs it: the query plan, drawn as a flow" onClick={() => explain(false)}>Explain</button>
				<MenuButton
					label="⋯"
					title="More"
					items={[
						{ label: "Save query…", hint: "in this browser", onClick: saveAs },
						{ label: "Create a view from it…", onClick: asView },
					]}
				/>
				<div className="menu" onClick={(e) => e.stopPropagation()}>
					<button onClick={() => setMenu(menu === "history" ? null : "history")}>History ▾</button>
					{menu === "history" && (
						<div className="menu-list wide">
							{hist.length ? hist.slice(0, 30).map((h, i) => (
								<button key={i} title={h.sql} onClick={() => { setText(h.sql); setMenu(null); }}>
									<span className="mono">{h.sql.replace(/\s+/g, " ").slice(0, 90)}</span>
									<span className="dim small"> {new Date(h.at).toLocaleString()}</span>
								</button>
							)) : <p className="dim small pad">Nothing run yet.</p>}
						</div>
					)}
				</div>
				<span className="grow" />
				<span className="dim small" title="embed('text') or embed('text', 'bge') is the text's vector, made in your browser">embed('…') works in SQL</span>
				<button className={`ghost icon${schemaOpen ? " pressed" : ""}`} title="Tables and columns" onClick={() => { setSchemaOpen(!schemaOpen); try { localStorage.setItem("musql-studio:schema-panel", schemaOpen ? "0" : "1"); } catch { /* kept for this page */ } }}><PanelRight size={15} /></button>
			</div>
			{ask && askOpen && <AskAI target={ask} onInsert={(q) => insert(q + "\n")} />}
			<div className="editor">
				<CodeMirror
					value={text}
					onChange={setText}
					onCreateEditor={(v) => { view.current = v; v.focus(); }}
					extensions={extensions}
					theme={dark ? "dark" : "light"}
					height="100%"
					placeholder="SELECT … — ⌘Enter runs the statement at the cursor (or the selection), ⇧⌘Enter runs them all. ORDER BY vector_distance_cos(col_emb, embed('a search')) ranks by meaning."
					basicSetup={{ foldGutter: false, highlightActiveLineGutter: false }}
				/>
				{schemaOpen && <SchemaPanel schema={schema} onInsert={insert} />}
			</div>
			{results.length > 0 && (
				<div className="rtabs">
					{(results.length > 1 || !results[0].columns?.length) && (
						<button className={`${shown === "summary" ? "on" : ""}${results.some((x) => x.error) ? " err" : ""}`} onClick={() => { setShown("summary"); setPlan(null); }}>
							<ListChecks size={12} /> Summary
						</button>
					)}
					{results.map((x, i) => x.columns?.length > 0 && (
						<button key={i} className={i === shown ? "on" : ""} title={x.sql} onClick={() => { setShown(i); setPlan(null); }}>
							{tabLabel(results, i)}
						</button>
					))}
				</div>
			)}
			{plan ? (
				<div className="result-meta">
					Plan of <span className="mono">{plan.sql.replace(/\s+/g, " ").slice(0, 80)}</span>
					<span className="grow" />
					{!plan.error && <button className="link" onClick={() => setPlanAs(planAs === "flow" ? "text" : "flow")}>{planAs === "flow" ? "as text" : "as flow"}</button>}
					{!plan.code && !plan.error && <button className="link" onClick={() => explain(true)}>bytecode</button>}
					<button className="link" onClick={() => setPlan(null)}>close</button>
				</div>
			) : r?.error ? (
				<div className="result-meta err">{r.error}</div>
			) : (
				<div className="result-meta">
					{shown === "summary" ? `${results.length} statement${results.length === 1 ? "" : "s"}` : r ? r.meta : "Run a statement to see its rows."}
					<span className="grow" />
					{runs.length > 1 && (
						<MenuButton
							className="ghost small"
							label={<><HistoryIcon size={13} /> {runAt === 0 ? "Latest run" : `${runAt} run${runAt > 1 ? "s" : ""} back`} ▾</>}
							title="Results of earlier runs"
							items={runs.map((x, k) => ({ label: x.sql.replace(/\s+/g, " ").slice(0, 60), hint: new Date(x.at).toLocaleTimeString(), onClick: () => { setRunAt(k); setShown(defaultShown(x.results)); setPlan(null); } }))}
						/>
					)}
					{r?.columns?.length > 0 && (
						<>
							<div className="seg small-seg">
								<button className={as === "grid" ? "on" : ""} onClick={() => setAs("grid")}>▦ Grid</button>
								<button className={as === "chart" ? "on" : ""} onClick={() => setAs("chart")}>▤ Chart</button>
							</div>
							<button className="link" onClick={() => download("result.csv", toCSV(r.columns, r.rows), "text/csv")}>CSV</button>
							<button className="link" onClick={() => download("result.json", toJSON(r.columns, r.rows), "application/json")}>JSON</button>
						</>
					)}
				</div>
			)}
			<div className="gridwrap">
				{plan ? (
					plan.error ? <div className="result-meta err">{plan.error}</div> : (
						<>
							{planAs === "flow" && !plan.code ? <PlanFlow rows={plan.rows} dark={dark} /> : <pre className="plan">{plan.tree}</pre>}
							{plan.code && <Grid columns={plan.code.columns} count={plan.code.rows.length} rowAt={(i) => plan.code.rows[i]} />}
						</>
					)
				) : busy ? (
					<Spinner label="Running…" />
				) : shown === "summary" ? (
					<Summary results={results} onOpen={(i) => setShown(i)} />
				) : r?.columns?.length > 0 && as === "chart" ? (
					<Chart key={r.sql} columns={r.columns} rows={r.rows} />
				) : r?.columns?.length > 0 && (
					<>
						<Grid key={r.sql} columns={r.columns} count={r.rows.length} rowAt={(i) => r.rows[i]} layoutKey={`q:${r.columns.join(",")}`} types={Object.fromEntries(r.columns.map((c, i) => [c, r.types?.[i] ?? ""]))} />
					</>
				)}
			</div>
		</>
	);
}

// defaultShown is what a run shows first: the summary when a statement
// failed or none returned rows, else the last that did.
function defaultShown(out) {
	const last = out.findLastIndex((r) => r.columns?.length);
	return out.some((r) => r.error) || last < 0 ? "summary" : last;
}

// tabLabel names result i: the table it reads, or "result N", with its rows
// and columns ("product 67×7").
function tabLabel(results, i) {
	const x = results[i];
	const from = /\bFROM\s+(?:"((?:[^"]|"")+)"|`([^`]+)`|\[([^\]]+)\]|([\w$]+))/i.exec(x.sql);
	const name = from ? (from[1]?.replace(/""/g, '"') ?? from[2] ?? from[3] ?? from[4]) : `result ${results.slice(0, i + 1).filter((y) => y.columns?.length).length}`;
	return `${name} ${fmt(x.rows.length)}×${x.columns.length}`;
}

// Summary lists every statement of a run: what it was, what it did, how long
// it took, and its error. A statement that returned rows opens its tab.
function Summary({ results, onOpen }) {
	return (
		<table className="summary">
			<thead><tr><th>#</th><th>statement</th><th>kind</th><th>result</th><th>time</th></tr></thead>
			<tbody>
				{results.map((x, i) => (
					<tr key={i} className={x.columns?.length ? "click" : ""} onClick={x.columns?.length ? () => onOpen(i) : undefined} title={x.sql}>
						<td className="dim">{i + 1}</td>
						<td className="sql">{x.sql.replace(/\s+/g, " ")}</td>
						<td>{x.verb}</td>
						<td className={x.error ? "err" : ""}>
							{x.error ? x.error : x.columns?.length ? `${fmt(x.rows.length)} row${x.rows.length === 1 ? "" : "s"} × ${x.columns.length}` : x.changes !== undefined ? `${fmt(x.changes)} changed` : "done"}
						</td>
						<td className="dim">{x.ms !== undefined ? `${x.ms.toFixed(2)} ms` : ""}</td>
					</tr>
				))}
			</tbody>
		</table>
	);
}

// planTree draws EXPLAIN QUERY PLAN's rows (id, parent, notused, detail) as
// the indented tree the sqlite3 shell prints.
function planTree(rows) {
	const kids = new Map();
	for (const [id, parent, , detail] of rows) {
		if (!kids.has(parent)) kids.set(parent, []);
		kids.get(parent).push({ id, detail });
	}
	const lines = ["QUERY PLAN"];
	const walk = (parent, prefix) => {
		const list = kids.get(parent) ?? [];
		list.forEach((n, i) => {
			const lastOne = i === list.length - 1;
			lines.push(`${prefix}${lastOne ? "`--" : "|--"}${n.detail}`);
			walk(n.id, prefix + (lastOne ? "   " : "|  "));
		});
	};
	walk(0, "");
	return lines.join("\n");
}

// SchemaPanel lists the tables and views beside the editor; a click puts a
// name in at the cursor.
function SchemaPanel({ schema, onInsert }) {
	const [open, setOpen] = useState({});
	const [cols, setCols] = useState({});
	const objs = schema.filter((o) => (o.type === "table" || o.type === "view") && !/^musql_studio_|_(data|idx|content|docsize|config)$/.test(o.name));
	async function toggle(name) {
		setOpen({ ...open, [name]: !open[name] });
		if (!cols[name]) {
			const r = await query(`PRAGMA table_info("${name.replace(/"/g, '""')}")`).catch(() => null);
			setCols((c) => ({ ...c, [name]: r ? r.rows.map((x) => ({ name: x[1], type: x[2], pk: x[5] > 0 })) : [] }));
		}
	}
	const q = (n) => (/^[A-Za-z_][A-Za-z0-9_]*$/.test(n) ? n : `"${n.replace(/"/g, '""')}"`);
	return (
		<aside className="schemapanel">
			<div className="sp-head">Schema</div>
			{objs.map((o) => (
				<div key={o.name}>
					<div className="sp-row">
						<button className="sp-toggle" onClick={() => toggle(o.name)}>{open[o.name] ? "▾" : "▸"}</button>
						<button className="sp-name" title="Insert the name" onClick={() => onInsert(q(o.name))}>{o.type === "view" ? "◇" : "▦"} {o.name}</button>
					</div>
					{open[o.name] && (cols[o.name] ?? []).map((c) => (
						<button key={c.name} className="sp-col" title={`Insert ${c.name}`} onClick={() => onInsert(q(c.name))}>
							<span>{c.pk ? "🔑 " : ""}{c.name}</span>
							<span className="dim">{(c.type || "").toLowerCase()}</span>
						</button>
					))}
				</div>
			))}
		</aside>
	);
}
