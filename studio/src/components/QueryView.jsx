import { useEffect, useMemo, useRef, useState } from "react";
import CodeMirror from "@uiw/react-codemirror";
import { EditorView, keymap } from "@codemirror/view";
import { Prec } from "@codemirror/state";
import { closeCompletion } from "@codemirror/autocomplete";
import { sql, SQLite } from "@codemirror/lang-sql";
import { call, query, run } from "../db.js";
import { download, fmt, toCSV, toJSON } from "../values.js";
import { useStudio } from "../studio.js";
import { DEFAULT_MODEL, embed, vectorBlob } from "../embed.js";
import { expandEmbeds, verb } from "../sqltext.js";
import { history, remember, save } from "../library.js";
import Grid from "./Grid.jsx";

const MAX_ROWS = 5000;
const READS = new Set(["SELECT", "WITH", "VALUES", "EXPLAIN", "PRAGMA"]);

/**
 * A SQL editor and its results. Ctrl/Cmd+Enter runs the selection, or all of
 * it: a script runs a statement at a time, each with its own result, and stops
 * at the first error. embed('text') in a statement is the text's vector
 * (embed.js), bound as a parameter.
 */
export default function QueryView({ initial = "", title, onTitle }) {
	const { schema, columnsOf, status, changed, dark } = useStudio();
	const [text, setText] = useState(initial);
	const [results, setResults] = useState([]);
	const [shown, setShown] = useState(0);
	const [plan, setPlan] = useState(null); // {sql, tree, code?}
	const [busy, setBusy] = useState(false);
	const [menu, setMenu] = useState(null); // "history" | null
	const view = useRef(null);
	const go = useRef(null);

	const target = () => {
		const st = view.current?.state;
		const sel = st ? st.sliceDoc(st.selection.main.from, st.selection.main.to) : "";
		return (sel.trim() ? sel : text).trim();
	};

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

	go.current = async () => {
		const q = target();
		if (!q) return;
		setBusy(true);
		setPlan(null);
		const out = [];
		let wrote = false;
		const t0 = performance.now();
		try {
			const stmts = await call("split", q);
			remember(q);
			for (const s of stmts) {
				const t = performance.now();
				try {
					const { sql, params } = await bind(s);
					if (READS.has(verb(sql))) {
						const r = await query(sql, params);
						out.push({ sql: s, ...r, meta: `${fmt(r.rows.length)} row${r.rows.length === 1 ? "" : "s"} · ${r.ms.toFixed(2)} ms` });
						if (verb(sql) === "PRAGMA") wrote = true; // a PRAGMA can set as well as read
					} else {
						const r = await run(sql, params);
						wrote = true;
						// Only INSERT, UPDATE, DELETE and REPLACE count rows: after any other
						// statement changes() still holds the last count before it.
						const counted = /^(INSERT|UPDATE|DELETE|REPLACE)$/.test(verb(sql));
						const ms = `${(performance.now() - t).toFixed(2)} ms`;
						out.push({ sql: s, columns: [], rows: [], meta: counted ? `${fmt(r.changes)} row${r.changes === 1 ? "" : "s"} changed · ${ms}` : `Done · ${ms}` });
					}
				} catch (e) {
					out.push({ sql: s, error: e.message });
					break;
				}
			}
		} catch (e) {
			out.push({ sql: q, error: e.message });
		} finally {
			setBusy(false);
		}
		setResults(out);
		// Show the last statement that returned rows, or else the last one.
		const pick = out.findLastIndex((r) => r.error) >= 0 ? out.length - 1 : Math.max(out.findLastIndex((r) => r.columns?.length), out.length - 1);
		setShown(pick);
		const failed = out.find((r) => r.error);
		const ms = (performance.now() - t0).toFixed(1);
		if (failed) status(out.length > 1 ? `Statement ${out.length} of the script failed: ${failed.error}` : failed.error, true);
		else status(out.length > 1 ? `${out.length} statements · ${ms} ms` : out[0]?.meta ?? "Done");
		if (wrote) changed();
	};

	async function explain(code) {
		const stmts = await call("split", target()).catch(() => []);
		const s = stmts[results.length ? Math.min(shown, stmts.length - 1) : 0] ?? stmts[0];
		if (!s) return;
		try {
			const { sql, params } = await bind(s);
			const qp = await query(`EXPLAIN QUERY PLAN ${sql}`, params);
			const bc = code ? await query(`EXPLAIN ${sql}`, params) : null;
			setPlan({ sql: s, tree: planTree(qp.rows), code: bc });
		} catch (e) {
			setPlan({ sql: s, error: e.message });
		}
	}

	function saveAs() {
		const q = target();
		if (!q) return;
		const name = prompt("Save this query as:", title?.startsWith("Query ") ? "" : title);
		if (!name) return;
		save(name, q);
		onTitle?.(name);
		status(`Saved “${name}”.`);
	}

	// Completion knows every table and view and their columns. Ctrl/Cmd+Enter is
	// taken before any keymap, so an open completion list cannot swallow it.
	const extensions = useMemo(() => {
		const tables = {};
		for (const o of schema) if (o.type === "table" || o.type === "view") tables[o.name] = columnsOf(o.name);
		const runKey = (v) => {
			closeCompletion(v);
			go.current();
			return true;
		};
		return [
			sql({ dialect: SQLite, schema: tables, upperCaseKeywords: true }),
			EditorView.lineWrapping,
			EditorView.domEventHandlers({
				keydown(e, v) {
					if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
						e.preventDefault();
						return runKey(v);
					}
					return false;
				},
			}),
			Prec.highest(keymap.of([{ key: "Mod-Enter", run: runKey }])),
		];
	}, [schema, columnsOf]);

	useEffect(() => {
		const close = () => setMenu(null);
		addEventListener("click", close);
		return () => removeEventListener("click", close);
	}, []);

	const r = results[shown];
	const hist = menu === "history" ? history() : [];

	return (
		<>
			<div className="toolbar">
				<button className="primary" disabled={busy} onClick={() => go.current()}>Run</button>
				<span className="kbd">⌘ Enter</span>
				<button title="The query plan of the statement" onClick={() => explain(false)}>Explain</button>
				<button title="Save this query, by name, in this browser" onClick={saveAs}>Save…</button>
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
			</div>
			<div className="editor">
				<CodeMirror
					value={text}
					onChange={setText}
					onCreateEditor={(v) => { view.current = v; v.focus(); }}
					extensions={extensions}
					theme={dark ? "dark" : "light"}
					height="100%"
					placeholder="SELECT … — ⌘/Ctrl+Enter runs the selection, or everything. ORDER BY vector_distance_cos(col_emb, embed('a search')) ranks by meaning."
					basicSetup={{ foldGutter: false, highlightActiveLineGutter: false }}
				/>
			</div>
			{results.length > 1 && (
				<div className="stmts">
					{results.map((x, i) => (
						<button key={i} className={`${i === shown ? "on" : ""}${x.error ? " err" : ""}`} title={x.sql} onClick={() => { setShown(i); setPlan(null); }}>
							{i + 1}. {verb(x.sql)} <span className="dim">{x.error ? "error" : x.meta.split(" · ")[0]}</span>
						</button>
					))}
				</div>
			)}
			{plan ? (
				<div className="result-meta">
					Plan of <span className="mono">{plan.sql.replace(/\s+/g, " ").slice(0, 80)}</span>
					<span className="grow" />
					{!plan.code && !plan.error && <button className="link" onClick={() => explain(true)}>bytecode</button>}
					<button className="link" onClick={() => setPlan(null)}>close</button>
				</div>
			) : r?.error ? (
				<div className="result-meta err">{r.error}</div>
			) : (
				<div className="result-meta">
					{r ? r.meta : "Run a statement to see its rows."}
					<span className="grow" />
					{r?.columns?.length > 0 && (
						<>
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
							<pre className="plan">{plan.tree}</pre>
							{plan.code && <Grid columns={plan.code.columns} rows={plan.code.rows} />}
						</>
					)
				) : r?.columns?.length > 0 && (
					<>
						<Grid columns={r.columns} rows={r.rows.slice(0, MAX_ROWS)} types={Object.fromEntries(r.columns.map((c, i) => [c, r.types?.[i] ?? ""]))} />
						{r.rows.length > MAX_ROWS && <p className="dim pad">Showing the first {fmt(MAX_ROWS)}.</p>}
					</>
				)}
			</div>
		</>
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
