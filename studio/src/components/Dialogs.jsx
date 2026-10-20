import { useEffect, useRef, useState } from "react";
import { ident, run } from "../db.js";
import { fmt } from "../values.js";
import { importRows, inferTypes, parseCSV, parseJSON } from "../importer.js";
import { TypeInput } from "./TableView.jsx";

function useModal() {
	const ref = useRef(null);
	useEffect(() => ref.current.showModal(), []);
	return ref;
}

/** The new-table form. onClose(name) once the table exists, onClose(null) otherwise. */
export function NewTableDialog({ onClose, status }) {
	const ref = useModal();
	const [name, setName] = useState("");
	const [cols, setCols] = useState([{ name: "id", type: "INTEGER", pk: true, notnull: false, dflt: "" }, { name: "", type: "TEXT", pk: false, notnull: false, dflt: "" }]);
	const set = (i, k, v) => setCols(cols.map((c, j) => (j === i ? { ...c, [k]: v } : c)));
	const used = cols.filter((c) => c.name.trim());
	const pks = used.filter((c) => c.pk);
	const ddl =
		`CREATE TABLE ${ident(name || "…")}(` +
		[
			...used.map((c) => `${ident(c.name)} ${c.type}${pks.length === 1 && c.pk ? " PRIMARY KEY" : ""}${c.notnull ? " NOT NULL" : ""}${c.dflt !== "" ? ` DEFAULT ${c.dflt}` : ""}`),
			...(pks.length > 1 ? [`PRIMARY KEY(${pks.map((c) => ident(c.name)).join(", ")})`] : []),
		].join(", ") +
		")";
	return (
		<dialog ref={ref} onClose={() => onClose(null)}>
			<form onSubmit={async (e) => {
				e.preventDefault();
				try {
					await run(ddl);
					onClose(name);
				} catch (err) {
					status(err.message, true);
				}
			}}>
				<h2>New table</h2>
				<div className="fields">
					<label className="field"><span>name</span><input autoFocus spellCheck={false} value={name} onChange={(e) => setName(e.target.value)} /></label>
				</div>
				<table className="grid coledit">
					<thead><tr><th>column</th><th>type</th><th>key</th><th>not null</th><th>default</th><th /></tr></thead>
					<tbody>
						{cols.map((c, i) => (
							<tr key={i}>
								<td><input spellCheck={false} value={c.name} placeholder="name" onChange={(e) => set(i, "name", e.target.value)} /></td>
								<td><TypeInput value={c.type} onChange={(v) => set(i, "type", v)} /></td>
								<td><input type="checkbox" checked={c.pk} onChange={(e) => set(i, "pk", e.target.checked)} /></td>
								<td><input type="checkbox" checked={c.notnull} onChange={(e) => set(i, "notnull", e.target.checked)} /></td>
								<td><input spellCheck={false} value={c.dflt} placeholder="none" onChange={(e) => set(i, "dflt", e.target.value)} /></td>
								<td><button type="button" className="link" onClick={() => setCols(cols.filter((_, j) => j !== i))}>×</button></td>
							</tr>
						))}
					</tbody>
				</table>
				<button type="button" onClick={() => setCols([...cols, { name: "", type: "TEXT", pk: false, notnull: false, dflt: "" }])}>＋ Column</button>
				<pre className="ddl small">{ddl}</pre>
				<div className="dialog-actions">
					<button type="button" onClick={() => onClose(null)}>Cancel</button>
					<button type="submit" className="primary" disabled={!name.trim() || !used.length}>Create</button>
				</div>
			</form>
		</dialog>
	);
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
