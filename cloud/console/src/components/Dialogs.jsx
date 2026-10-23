import { useEffect, useRef, useState } from "react";
import { rows, run } from "../db.js";
import { applyEdit, blankColumn, blankTable, createSQL, previewEdit, readTable } from "../schema.js";
import { fmt } from "../values.js";
import { importRows, inferTypes, parseCSV, parseJSON } from "../importer.js";
import { TypeInput } from "./TableView.jsx";
import Drawer from "./Drawer.jsx";

const db = { run, rows };

function useModal() {
	const ref = useRef(null);
	useEffect(() => ref.current.showModal(), []);
	return ref;
}

const ON_DELETE = ["", "CASCADE", "SET NULL", "SET DEFAULT", "RESTRICT"];

/**
 * The table form: a new table, or (with table) an existing one's columns,
 * keys, defaults and foreign keys. onClose(name) once the change is made,
 * onClose(null) otherwise. tables lists the tables a foreign key can name.
 */
export function TableDialog({ table, tables = [], onClose, onSQL, status }) {
	const [old, setOld] = useState(table ? null : blankTable());
	const [m, setM] = useState(old);
	const [busy, setBusy] = useState(false);
	useEffect(() => {
		if (table) readTable(db, table).then((t) => { setOld(t); setM(structuredClone(t)); }, (e) => status(e.message, true));
	}, [table]);
	if (!m) return <Drawer wide title={`Edit ${table}`} onClose={() => onClose(null)}><p className="dim">Reading {table}…</p></Drawer>;

	const set = (i, k, v) => setM({ ...m, cols: m.cols.map((c, j) => (j === i ? { ...c, [k]: v } : c)) });
	const move = (i, d) => {
		const cols = [...m.cols];
		[cols[i], cols[i + d]] = [cols[i + d], cols[i]];
		setM({ ...m, cols });
	};
	const used = m.cols.filter((c) => c.name.trim());
	const dup = used.find((c, i) => used.findIndex((x) => x.name.toLowerCase() === c.name.toLowerCase()) !== i);
	const script = table ? previewEdit(old, m) : [createSQL(m)];
	const blocked = table && old.unmodeled.length > 0;
	const nothing = table && !script.length;
	const problem = !m.name.trim() ? "The table needs a name." : !used.length ? "The table needs a column." : dup ? `Two columns are named ${dup.name}.` : m.withoutRowid && !used.some((c) => c.pk) ? "A WITHOUT ROWID table needs a primary key." : null;

	async function submit(e) {
		e?.preventDefault();
		setBusy(true);
		try {
			if (table) await applyEdit(db, old, m);
			else await run(createSQL(m));
			onClose(m.name);
		} catch (err) {
			status(err.message, true);
			setBusy(false);
		}
	}

	return (
		<Drawer wide title={table ? `Edit table ${table}` : "Create a table"} subtitle={table ? "Columns, keys and foreign keys" : "Name it, then add its columns"} onClose={() => onClose(null)}>
			<form onSubmit={submit}>
				{blocked ? (
					<>
						<p className="warn small">
							{table} has {old.unmodeled.join(", ")}, which this form cannot show, so a change made here would lose it. Change it with SQL instead: the
							query tab opens with its definition and the steps of a rebuild.
						</p>
						<pre className="ddl small">{createSQL(old)}</pre>
						<div className="dialog-actions">
							<button type="button" onClick={() => onClose(null)}>Cancel</button>
							<button type="button" className="primary" onClick={() => { onSQL(rebuildTemplate(table)); onClose(null); }}>Edit as SQL</button>
						</div>
					</>
				) : (
					<>
						<div className="fields">
							<label className="field"><span>name</span><input autoFocus={!table} spellCheck={false} value={m.name} onChange={(e) => setM({ ...m, name: e.target.value })} /></label>
						</div>
						<div className="coledit-wrap">
							<table className="grid coledit">
								<thead>
									<tr>
										<th /><th>column</th><th>type</th>
										<th title="Primary key">key</th><th title="NOT NULL">not null</th><th title="UNIQUE">unique</th>
										<th>default</th><th title="A foreign key: table, or table(column)">references</th><th>on delete</th><th />
									</tr>
								</thead>
								<tbody>
									{m.cols.map((c, i) => (
										<tr key={i} className={c.orig == null && table ? "added" : ""}>
											<td className="moves">
												<button type="button" className="link" disabled={i === 0} title="Move up" onClick={() => move(i, -1)}>↑</button>
												<button type="button" className="link" disabled={i === m.cols.length - 1} title="Move down" onClick={() => move(i, 1)}>↓</button>
											</td>
											<td><input spellCheck={false} value={c.name} placeholder="name" title={c.orig && c.orig !== c.name ? `was ${c.orig}` : undefined} onChange={(e) => set(i, "name", e.target.value)} /></td>
											<td><TypeInput value={c.type} onChange={(v) => set(i, "type", v)} /></td>
											<td className="ck"><input type="checkbox" checked={c.pk} onChange={(e) => set(i, "pk", e.target.checked)} /></td>
											<td className="ck"><input type="checkbox" checked={c.notnull} onChange={(e) => set(i, "notnull", e.target.checked)} /></td>
											<td className="ck"><input type="checkbox" checked={c.unique} onChange={(e) => set(i, "unique", e.target.checked)} /></td>
											<td><input spellCheck={false} value={c.dflt} placeholder="none" onChange={(e) => set(i, "dflt", e.target.value)} /></td>
											<td><input spellCheck={false} list="fktables" value={c.ref} placeholder="none" onChange={(e) => set(i, "ref", e.target.value)} /></td>
											<td>
												<select value={c.onDelete} disabled={!c.ref.trim()} onChange={(e) => set(i, "onDelete", e.target.value)}>
													{ON_DELETE.map((o) => <option key={o} value={o}>{o || "no action"}</option>)}
												</select>
											</td>
											<td><button type="button" className="link" title={c.orig ? `Drop ${c.orig}: its values are lost` : "Remove"} onClick={() => setM({ ...m, cols: m.cols.filter((_, j) => j !== i) })}>×</button></td>
										</tr>
									))}
								</tbody>
							</table>
							<datalist id="fktables">{tables.map((t) => <option key={t} value={t} />)}</datalist>
						</div>
						<div className="inline">
							<button type="button" onClick={() => setM({ ...m, cols: [...m.cols, blankColumn()] })}>＋ Column</button>
							<span className="grow" />
							<label className="check"><input type="checkbox" checked={m.withoutRowid} onChange={(e) => setM({ ...m, withoutRowid: e.target.checked })} />WITHOUT ROWID</label>
							<label className="check" title="Values must match their column's type"><input type="checkbox" checked={m.strict} onChange={(e) => setM({ ...m, strict: e.target.checked })} />STRICT</label>
						</div>
						{table && script.some((s) => s.startsWith("DROP TABLE")) && (
							<p className="dim small">This change rebuilds {table}: a new table, the rows copied in, the old one dropped, all in one transaction that rolls back if any row or index no longer fits.</p>
						)}
						<pre className="ddl small">{nothing ? "-- no change" : script.join(";\n\n")}</pre>
						{problem && <p className="warn small">{problem}</p>}
						<div className="dialog-actions">
							{onSQL && <button type="button" className="ghost" title="Open these statements in a query tab instead" onClick={() => { onSQL(script.join(";\n\n") + ";"); onClose(null); }}>Open as SQL</button>}
							<span className="grow" />
							<button type="button" onClick={() => onClose(null)}>Cancel</button>
							<button type="submit" className="primary" disabled={!!problem || nothing || busy}>{busy ? "Working…" : table ? "Apply" : "Create"}</button>
						</div>
					</>
				)}
			</form>
		</Drawer>
	);
}

// The SQL a hand edit of a table the form cannot hold starts from: SQLite's
// rebuild steps around the table's definition.
function rebuildTemplate(name) {
	return `-- Rebuild ${name}: edit the new definition, keep the column lists in step.
PRAGMA foreign_keys = OFF;
BEGIN;
-- CREATE TABLE "${name}__new"(…);   -- the definition, changed
-- INSERT INTO "${name}__new"(…) SELECT … FROM "${name}";
-- DROP TABLE "${name}";
-- ALTER TABLE "${name}__new" RENAME TO "${name}";
-- …then create its indexes and triggers again (Structure lists them)
SELECT sql FROM sqlite_schema WHERE tbl_name = '${name.replace(/'/g, "''")}';
-- PRAGMA foreign_key_check; COMMIT; PRAGMA foreign_keys = ON;
`;
}

/**
 * Imports a CSV or JSON file into a new table or an existing one. onClose(table)
 * once imported, onClose(null) otherwise.
 */
export function ImportDialog({ file: dropped, tables, onClose, status }) {
	const ref = useModal();
	const [file, setFile] = useState(null);
	const [parsed, setParsed] = useState(null); // {columns, rows}
	const [header, setHeader] = useState(true);
	const [raw, setRaw] = useState(null); // CSV rows, before the header choice
	const [table, setTable] = useState("");
	const [busy, setBusy] = useState(false);
	useEffect(() => {
		if (dropped) pick(dropped);
	}, [dropped]);

	async function pick(f) {
		setFile(f);
		if (!f) return;
		setTable(f.name.replace(/\.[^.]+$/, "").replace(/[^\p{L}\p{N}_]+/gu, "_"));
		try {
			const text = await f.text();
			if (/\.json$/i.test(f.name) || /^\s*[[{]/.test(text)) {
				setRaw(null);
				setParsed(parseJSON(text));
			} else {
				const rows = parseCSV(text);
				setRaw(rows);
				setParsed(null);
			}
		} catch (e) {
			status(`Could not read ${f.name}: ${e.message}`, true);
		}
	}

	const data = raw
		? header
			? { columns: (raw[0] ?? []).map((c, i) => c.trim() || `c${i + 1}`), rows: raw.slice(1) }
			: { columns: (raw[0] ?? []).map((_, i) => `c${i + 1}`), rows: raw }
		: parsed;
	const types = data ? inferTypes(data.columns, data.rows) : [];
	const exists = tables.includes(table);

	return (
		<dialog ref={ref} onClose={() => onClose(null)}>
			<form onSubmit={async (e) => {
				e.preventDefault();
				setBusy(true);
				try {
					await importRows(table, data.columns, types, data.rows, !exists, (n) => status(`Importing… ${fmt(n)} of ${fmt(data.rows.length)}`));
					status(`Imported ${fmt(data.rows.length)} rows into ${table}.`);
					onClose(table);
				} catch (err) {
					status(err.message, true);
					setBusy(false);
				}
			}}>
				<h2>Import CSV or JSON</h2>
				<div className="fields">
					<label className="field"><span>file</span>{dropped ? <span>{dropped.name}</span> : <input type="file" accept=".csv,.tsv,.txt,.json" onChange={(e) => pick(e.target.files[0])} />}</label>
					{raw && <label className="check"><input type="checkbox" checked={header} onChange={(e) => setHeader(e.target.checked)} />The first line holds the column names</label>}
					<label className="field"><span>into table</span><input spellCheck={false} list="tables" value={table} onChange={(e) => setTable(e.target.value)} /></label>
					<datalist id="tables">{tables.map((t) => <option key={t} value={t} />)}</datalist>
				</div>
				{data && (
					<p className="dim small">
						{fmt(data.rows.length)} rows · {exists ? `appended to the existing ${table}` : `a new table: ${data.columns.map((c, i) => `${c} ${types[i]}`).join(", ")}`}
					</p>
				)}
				<div className="dialog-actions">
					<button type="button" onClick={() => onClose(null)}>Cancel</button>
					<button type="submit" className="primary" disabled={!data || !table.trim() || busy || !file}>Import</button>
				</div>
			</form>
		</dialog>
	);
}
