import { useMemo, useRef, useState } from "react";
import CodeMirror from "@uiw/react-codemirror";
import { EditorView, keymap } from "@codemirror/view";
import { Prec } from "@codemirror/state";
import { sql, SQLite } from "@codemirror/lang-sql";
import { query } from "../db.js";
import { fmt } from "../values.js";
import { useStudio } from "../studio.js";
import Grid from "./Grid.jsx";

const MAX_ROWS = 5000;
const READS = /^\s*(SELECT|WITH|VALUES|EXPLAIN)\b/i;

/** A SQL editor and its result. Ctrl/Cmd+Enter runs the selection, or all of it. */
export default function QueryView({ initial = "" }) {
	const { schema, columnsOf, status, changed, dark } = useStudio();
	const [text, setText] = useState(initial);
	const [result, setResult] = useState(null);
	const [busy, setBusy] = useState(false);
	const view = useRef(null);
	const go = useRef(null);

	go.current = async () => {
		const st = view.current?.state;
		const sel = st ? st.sliceDoc(st.selection.main.from, st.selection.main.to) : "";
		const q = (sel.trim() ? sel : text).trim();
		if (!q) return;
		setBusy(true);
		try {
			const r = await query(q);
			const ms = `${r.ms.toFixed(2)} ms`;
			const meta = r.columns.length ? `${fmt(r.rows.length)} row${r.rows.length === 1 ? "" : "s"} · ${ms}` : r.script ? `Script ran · ${ms}` : `Done · ${ms}`;
			setResult({ ...r, meta });
			status(meta);
			if (r.script || !READS.test(q)) changed();
		} catch (e) {
			setResult({ error: e.message });
			status(e.message, true);
		} finally {
			setBusy(false);
		}
	};

	// Completion knows every table and view and their columns.
	const extensions = useMemo(() => {
		const tables = {};
		for (const o of schema) if (o.type === "table" || o.type === "view") tables[o.name] = columnsOf(o.name);
		return [
			sql({ dialect: SQLite, schema: tables, upperCaseKeywords: true }),
			EditorView.lineWrapping,
			Prec.highest(keymap.of([{ key: "Mod-Enter", run: () => (go.current(), true) }])),
		];
	}, [schema, columnsOf]);

	return (
		<>
			<div className="toolbar">
				<button className="primary" disabled={busy} onClick={() => go.current()}>Run</button>
				<span className="kbd">⌘ Enter</span>
				<span className="grow" />
			</div>
			<div className="editor">
				<CodeMirror
					value={text}
					onChange={setText}
					onCreateEditor={(v) => { view.current = v; v.focus(); }}
					extensions={extensions}
					theme={dark ? "dark" : "light"}
					height="100%"
					placeholder="SELECT … — ⌘/Ctrl+Enter runs the selection, or everything"
					basicSetup={{ foldGutter: false, highlightActiveLineGutter: false }}
				/>
			</div>
			{result?.error ? (
				<div className="result-meta err">{result.error}</div>
			) : (
				<div className="result-meta">{result ? result.meta : "Run a statement to see its rows."}</div>
			)}
			<div className="gridwrap">
				{result?.columns?.length > 0 && (
					<>
						<Grid columns={result.columns} rows={result.rows.slice(0, MAX_ROWS)} />
						{result.rows.length > MAX_ROWS && <p className="dim pad">Showing the first {fmt(MAX_ROWS)}.</p>}
					</>
				)}
			</div>
		</>
	);
}
