import { useEffect, useState } from "react";
import Drawer from "./Drawer.jsx";
import { ident, rows, run } from "../db.js";
import { applyEdit, blankColumn, previewEdit, readTable } from "../schema.js";

const db = { run, rows };

export const TYPES = [
	["INTEGER", "whole numbers, ids"],
	["TEXT", "strings"],
	["REAL", "floating-point numbers"],
	["NUMERIC", "numbers kept exact when they can be"],
	["BLOB", "bytes"],
	["BOOLEAN", "0 or 1"],
	["DATETIME", "dates and times, as text"],
	["JSON", "JSON, as text"],
	["F32_BLOB(384)", "a vector"],
];

/**
 * One column in a side panel: add one (column null) or change one — name,
 * type, default, key, NULLs, UNIQUE and its foreign key. The change runs
 * through applyEdit, so it is an ALTER or a rebuild in one transaction.
 */
export default function ColumnPanel({ table, column, tables, onClose, status }) {
	const [old, setOld] = useState(null);
	const [c, setC] = useState(null);
	const [refCols, setRefCols] = useState([]);
	const [busy, setBusy] = useState(false);
	useEffect(() => {
		readTable(db, table).then((t) => {
			setOld(t);
			setC(column ? { ...t.cols.find((x) => x.orig === column) } : blankColumn());
		}, (e) => status(e.message, true));
	}, [table, column]);
	const refTable = c?.ref.replace(/\(.*$/, "").trim() ?? "";
	useEffect(() => {
		if (refTable) rows(`PRAGMA table_info(${ident(refTable)})`).then((r) => setRefCols(r.map((x) => x.name)), () => setRefCols([]));
	}, [refTable]);

	if (!old || !c) return <Drawer title={column ? `Column ${column}` : "Add a column"} onClose={onClose}><p className="dim">Reading {table}…</p></Drawer>;

	const next = { ...old, cols: column ? old.cols.map((x) => (x.orig === column ? c : x)) : [...old.cols, c] };
	const set = (k, v) => setC({ ...c, [k]: v });
	const refCol = /\((.*)\)/.exec(c.ref)?.[1] ?? "";
	const setRef = (t, col) => setC({ ...c, ref: t ? (col ? `${t}(${col})` : t) : "", onDelete: t ? c.onDelete : "" });
	const dup = old.cols.some((x) => x.orig !== column && x.name.toLowerCase() === c.name.trim().toLowerCase());
	const problem = !c.name.trim() ? "The column needs a name." : dup ? `${table} already has a column ${c.name}.` : old.unmodeled.length ? `${table} has ${old.unmodeled.join(", ")}: change it with SQL.` : null;
	const script = problem ? [] : previewEdit(old, next);

	async function apply(model) {
		setBusy(true);
		try {
			await applyEdit(db, old, model);
			onClose(true);
		} catch (e) {
			status(e.message, true);
			setBusy(false);
		}
	}

	return (
		<Drawer
			title={column ? `Edit column` : "Add a column"}
			subtitle={column ? `${table}.${column}` : `to ${table}`}
			onClose={() => onClose(false)}
			footer={
				<>
					{column && <button className="danger" disabled={busy} onClick={() => confirm(`Delete the column ${column}? Its values are lost.`) && apply({ ...old, cols: old.cols.filter((x) => x.orig !== column) })}>Delete column</button>}
					<span className="grow" />
					<button onClick={() => onClose(false)}>Cancel</button>
					<button className="primary" disabled={busy || !!problem || !script.length} onClick={() => apply(next)}>{busy ? "Saving…" : "Save"}</button>
				</>
			}
		>
			<div className="form">
				<Field label="Name"><input value={c.name} spellCheck={false} placeholder="column_name" onChange={(e) => set("name", e.target.value)} /></Field>
				<Field label="Type" help={TYPES.find(([t]) => t === c.type.toUpperCase())?.[1] ?? "any type name; SQLite stores by affinity"}>
					<input list="coltypes2" value={c.type} spellCheck={false} onChange={(e) => set("type", e.target.value)} />
					<datalist id="coltypes2">{TYPES.map(([t, d]) => <option key={t} value={t}>{d}</option>)}</datalist>
				</Field>
				<Field label="Default value" help="A literal like 0 or 'draft', or an expression like CURRENT_TIMESTAMP">
					<input value={c.dflt} spellCheck={false} placeholder="NULL" onChange={(e) => set("dflt", e.target.value)} />
				</Field>
				<div className="toggles">
					<Toggle label="Primary key" help="Identifies each row" checked={c.pk} onChange={(v) => set("pk", v)} />
					<Toggle label="Allow empty (NULL)" help="Off makes the value required" checked={!c.notnull} onChange={(v) => set("notnull", !v)} />
					<Toggle label="Unique" help="No two rows share a value" checked={c.unique} onChange={(v) => set("unique", v)} />
				</div>
				<h3>Foreign key</h3>
				<p className="dim small">Link this column to a row of another table.</p>
				<div className="fkrow">
					<select value={refTable} onChange={(e) => setRef(e.target.value, "")}>
						<option value="">No link</option>
						{tables.map((t) => <option key={t}>{t}</option>)}
					</select>
					{refTable && (
						<select value={refCol} onChange={(e) => setRef(refTable, e.target.value)}>
							<option value="">its primary key</option>
							{refCols.map((x) => <option key={x}>{x}</option>)}
						</select>
					)}
				</div>
				{refTable && (
					<Field label="When that row is deleted">
						<select value={c.onDelete} onChange={(e) => set("onDelete", e.target.value)}>
							<option value="">refuse while it is linked (no action)</option>
							<option value="CASCADE">delete this row too (cascade)</option>
							<option value="SET NULL">set this column to NULL</option>
							<option value="SET DEFAULT">set this column to its default</option>
							<option value="RESTRICT">refuse (restrict)</option>
						</select>
					</Field>
				)}
				{problem && <p className="warn small">{problem}</p>}
				{script.length > 0 && (
					<details className="sqlpreview">
						<summary>SQL that will run{script.some((s) => s.startsWith("DROP TABLE")) ? " · rebuilds the table, keeping its rows" : ""}</summary>
						<pre className="ddl small">{script.join(";\n\n")};</pre>
					</details>
				)}
			</div>
		</Drawer>
	);
}

export function Field({ label, help, children }) {
	return (
		<label className="ffield">
			<span className="flabel2">{label}</span>
			{children}
			{help && <span className="fhelp">{help}</span>}
		</label>
	);
}

export function Toggle({ label, help, checked, onChange }) {
	return (
		<label className="toggle">
			<input type="checkbox" checked={checked} onChange={(e) => onChange(e.target.checked)} />
			<span className="switch" />
			<span>
				{label}
				{help && <span className="fhelp">{help}</span>}
			</span>
		</label>
	);
}
