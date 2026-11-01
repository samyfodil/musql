import { useEffect, useState } from "react";

const OPS = [
	["=", "is"],
	["!=", "is not"],
	[">", ">"],
	[">=", "≥"],
	["<", "<"],
	["<=", "≤"],
	["contains", "contains"],
	["starts", "starts with"],
	["null", "is empty"],
	["notnull", "is not empty"],
];
const NOVALUE = new Set(["null", "notnull"]);

const ident = (s) => `"${String(s).replace(/"/g, '""')}"`;

/**
 * whereOf turns [{col, op, value}] into {sql, params} with every value bound;
 * a number typed against a numeric column compares as a number.
 */
export function whereOf(filters, types = {}) {
	const parts = [];
	const params = [];
	for (const f of filters) {
		if (!f.col) continue;
		const c = ident(f.col);
		if (f.op === "null") parts.push(`${c} IS NULL`);
		else if (f.op === "notnull") parts.push(`${c} IS NOT NULL`);
		else if (f.value === "") continue;
		else if (f.op === "contains" || f.op === "starts") {
			parts.push(`${c} LIKE ? ESCAPE '\\'`);
			const v = f.value.replace(/[\\%_]/g, "\\$&");
			params.push(f.op === "contains" ? `%${v}%` : `${v}%`);
		} else {
			parts.push(`${c} ${f.op === "!=" ? "IS NOT" : f.op === "=" ? "IS" : f.op} ?`);
			const numeric = /INT|REAL|FLOA|DOUB|NUM|DEC/i.test(types[f.col] ?? "") || types[f.col] === "";
			params.push(numeric && f.value.trim() !== "" && !isNaN(+f.value) ? +f.value : f.value);
		}
	}
	return { sql: parts.join(" AND "), params };
}

/** The filter row: column, operator and value, ANDed, or a hand-typed WHERE clause; onApply({filters} or {sql}). */
export default function FilterBar({ columns, filters, sql, onApply }) {
	const [rows, setRows] = useState(filters);
	const [raw, setRaw] = useState(sql ?? "");
	const [mode, setMode] = useState(sql ? "sql" : "build");
	useEffect(() => setRows(filters), [filters]);
	const set = (i, k, v) => setRows(rows.map((r, j) => (j === i ? { ...r, [k]: v } : r)));
	const apply = (rs = rows) => onApply({ filters: rs.filter((r) => r.col) });
	const add = () => setRows([...rows, { col: columns[0], op: "=", value: "" }]);
	const active = filters.length > 0 || !!sql;

	if (mode === "sql")
		return (
			<div className="filterbar">
				<span className="flabel">WHERE</span>
				<input className="where" autoFocus spellCheck={false} placeholder="plays > 50000 AND title LIKE 'Track 1%'   ⏎ to apply" value={raw} onChange={(e) => setRaw(e.target.value)} onKeyDown={(e) => e.key === "Enter" && onApply({ sql: raw.trim() })} />
				{sql && <button className="link" onClick={() => { setRaw(""); onApply({ sql: "" }); }}>clear</button>}
				<button className="link" title="Build the filter from menus instead" onClick={() => { setMode("build"); onApply({ filters: [] }); }}>use menus</button>
			</div>
		);

	return (
		<div className="filterbar">
			{!rows.length && (
				<button className="ghost addfilter" onClick={add}>
					<span className="ficon">⏷</span> Filter rows…
				</button>
			)}
			{rows.map((r, i) => (
				<div key={i} className="fchip">
					<select value={r.col} onChange={(e) => set(i, "col", e.target.value)}>
						{columns.map((c) => <option key={c}>{c}</option>)}
					</select>
					<select value={r.op} onChange={(e) => { const next = rows.map((x, j) => (j === i ? { ...x, op: e.target.value } : x)); setRows(next); if (NOVALUE.has(e.target.value)) apply(next); }}>
						{OPS.map(([v, l]) => <option key={v} value={v}>{l}</option>)}
					</select>
					{!NOVALUE.has(r.op) && (
						<input autoFocus={!r.value} spellCheck={false} placeholder="value" value={r.value} onChange={(e) => set(i, "value", e.target.value)} onKeyDown={(e) => e.key === "Enter" && apply()} onBlur={() => apply()} />
					)}
					<button className="link x" title="Remove this condition" onClick={() => { const next = rows.filter((_, j) => j !== i); setRows(next); apply(next); }}>×</button>
				</div>
			))}
			{rows.length > 0 && <button className="link" onClick={add}>＋ and</button>}
			{active && <button className="link" onClick={() => { setRows([]); onApply({ filters: [] }); }}>clear</button>}
			<span className="grow" />
			<button className="link dim" title="Type a WHERE clause instead" onClick={() => setMode("sql")}>SQL</button>
		</div>
	);
}
