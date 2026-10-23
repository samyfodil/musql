import { useState } from "react";
import Drawer from "./Drawer.jsx";

const long = (type, v) => /CHAR|CLOB|TEXT|JSON/i.test(type ?? "") && (v == null || String(v).length > 40 || String(v).includes("\n"));

/**
 * A row in a side panel: every column as a field, to insert a row (row null)
 * or change one. onSave({column: text | null}) gets only the columns that
 * changed (for an insert, those filled in); the caller stores each by its
 * column's type.
 */
export default function RowPanel({ name, cols, row, readOnly, onSave, onClose }) {
	const init = Object.fromEntries(cols.map((c) => [c.name, row ? (row[c.name] === null ? null : row[c.name] instanceof Uint8Array ? undefined : String(row[c.name])) : ""]));
	const [v, setV] = useState(init);
	const [busy, setBusy] = useState(false);
	const changed = cols.filter((c) => v[c.name] !== init[c.name] && !(row == null && v[c.name] === ""));
	const save = async () => {
		setBusy(true);
		const ok = await onSave(Object.fromEntries(changed.map((c) => [c.name, v[c.name]])));
		if (!ok) setBusy(false);
	};
	return (
		<Drawer
			title={readOnly ? `Row of ${name}` : row ? `Edit row` : `Insert a row`}
			subtitle={row ? name : `into ${name} · empty fields take their default`}
			onClose={onClose}
			footer={
				<>
					<span className="dim small">{readOnly ? "Read-only" : changed.length ? `${changed.length} field${changed.length > 1 ? "s" : ""} changed` : row ? "No changes" : ""}</span>
					<span className="grow" />
					<button onClick={onClose}>{readOnly ? "Close" : "Cancel"}</button>
					{!readOnly && <button className="primary" disabled={busy || (row && !changed.length)} onClick={save}>{busy ? "Saving…" : row ? "Save" : "Insert"}</button>}
				</>
			}
		>
			<form className="rowform" onSubmit={(e) => { e.preventDefault(); save(); }}>
				{cols.map((c) => {
					const val = v[c.name];
					const isNull = val === null;
					const blob = val === undefined;
					const set = (x) => setV({ ...v, [c.name]: x });
					return (
						<label key={c.name} className={`rowfield${val !== init[c.name] ? " dirty" : ""}`}>
							<div className="rowlabel">
								<span className="cname">{c.name}</span>
								<span className="badge">{(c.type || "any").toLowerCase()}</span>
								{c.pk > 0 && <span className="badge key">primary key</span>}
								{c.notnull && !c.pk ? <span className="badge">required</span> : null}
								<span className="grow" />
								{!readOnly && !blob && !c.pk && (
									<button type="button" className={`nullbtn${isNull ? " on" : ""}`} title="Set to NULL" onClick={() => set(isNull ? "" : null)}>NULL</button>
								)}
							</div>
							{blob ? (
								<div className="dim small fieldnote">A BLOB: change it with SQL.</div>
							) : long(c.type, val) ? (
								<textarea rows={4} disabled={readOnly || isNull} spellCheck={false} placeholder={isNull ? "NULL" : placeholder(c, row)} value={isNull ? "" : val} onChange={(e) => set(e.target.value)} />
							) : (
								<input disabled={readOnly || isNull} spellCheck={false} placeholder={isNull ? "NULL" : placeholder(c, row)} value={isNull ? "" : val} onChange={(e) => set(e.target.value)} />
							)}
						</label>
					);
				})}
				<button type="submit" hidden />
			</form>
		</Drawer>
	);
}

function placeholder(c, row) {
	if (row) return "empty text";
	if (c.pk && /INT/i.test(c.type)) return "automatic";
	if (c.dflt_value != null) return `default: ${c.dflt_value}`;
	return c.notnull ? "required" : "NULL";
}
