import { useCallback, useEffect, useMemo, useState } from "react";
import { ident, query, rows as rowsOf, run } from "../db.js";
import { fmt, show } from "../values.js";
import { useStudio } from "../studio.js";
import Grid from "./Grid.jsx";
import RowDialog from "./RowDialog.jsx";

const PAGE = 100;

/** One table or view: its rows a page at a time, editable, and its structure. */
export default function TableView({ name, mode: initialMode = "data" }) {
	const { schema, status, changed } = useStudio();
	const obj = schema.find((o) => o.name === name);
	const [mode, setMode] = useState(initialMode);
	const [cols, setCols] = useState([]);
	const [where, setWhere] = useState("");
	const [draft, setDraft] = useState("");
	const [sort, setSort] = useState(null);
	const [desc, setDesc] = useState(false);
	const [offset, setOffset] = useState(0);
	const [data, setData] = useState(null);
	const [total, setTotal] = useState(0);
	const [error, setError] = useState(null);
	const [picked, setPicked] = useState(new Set());
	const [adding, setAdding] = useState(false);

	useEffect(() => setMode(initialMode), [initialMode]);

	const sql = obj?.sql ?? "";
	const view = obj?.type === "view";
	const virtual = /^\s*CREATE\s+VIRTUAL\b/i.test(sql);
	const withoutRowid = /\bWITHOUT\s+ROWID\b/i.test(sql);
	const pk = useMemo(() => cols.filter((c) => c.pk > 0).sort((a, b) => a.pk - b.pk).map((c) => c.name), [cols]);
	// A row is named by its rowid, or by its primary key in a WITHOUT ROWID
	// table. A view's rows have no name, so a view is read-only.
	const keyCols = withoutRowid ? pk : ["rowid"];
	const keyed = !view && keyCols.length > 0;
	const editable = keyed && !virtual;

	useEffect(() => {
		rowsOf(`PRAGMA table_info(${ident(name)})`).then(setCols, (e) => status(e.message, true));
	}, [name, sql]);

	const load = useCallback(async () => {
		const filter = where ? ` WHERE ${where}` : "";
		const order = sort ? ` ORDER BY ${ident(sort)}${desc ? " DESC" : ""}` : "";
		const key = keyed ? keyCols.map((k, i) => `${ident(k)} AS "__k${i}"`).join(", ") + ", " : "";
		try {
			const t = performance.now();
			const n = (await query(`SELECT count(*) FROM ${ident(name)}${filter}`)).rows[0][0];
			const r = await query(`SELECT ${key}* FROM ${ident(name)}${filter}${order} LIMIT ${PAGE} OFFSET ${offset}`);
			setTotal(Number(n));
			setData(r);
			setError(null);
			setPicked(new Set());
			status(`${name}: ${fmt(n)} rows · ${(performance.now() - t).toFixed(1)} ms`);
		} catch (e) {
			setError(e.message);
			status(e.message, true);
		}
	}, [name, where, sort, desc, offset, keyed, keyCols.join("\0")]);

	useEffect(() => {
		if (cols.length || view) load();
	}, [load, cols]);

	const nk = keyed ? keyCols.length : 0;
	const shown = data ? data.columns.slice(nk) : [];
	const body = data ? data.rows.map((r) => r.slice(nk)) : [];
	const keyOf = (i) => data.rows[i].slice(0, nk);
	const keyWhere = keyCols.map((k) => `${ident(k)} IS ?`).join(" AND ");
	const types = Object.fromEntries(cols.map((c) => [c.name, c.type]));

	async function write(sql, params, msg) {
		try {
			await run(sql, params);
			status(msg);
			changed(name);
			await load();
		} catch (e) {
			status(e.message, true);
		}
	}

	async function edit(i, col, value) {
		if (body[i][shown.indexOf(col)] instanceof Uint8Array) {
			status("BLOB values are edited with SQL, in a query tab.", true);
			return;
		}
		await write(`UPDATE ${ident(name)} SET ${ident(col)} = ? WHERE ${keyWhere}`, [value, ...keyOf(i)], `Updated ${col}.`);
	}

	async function remove() {
		const list = [...picked];
		if (!confirm(`Delete ${list.length} row${list.length > 1 ? "s" : ""} from ${name}?`)) return;
		try {
			for (const i of list) await run(`DELETE FROM ${ident(name)} WHERE ${keyWhere}`, keyOf(i));
			status(`Deleted ${list.length} row${list.length > 1 ? "s" : ""}.`);
		} catch (e) {
			status(e.message, true);
		}
		changed(name);
		await load();
	}

	async function insert(values) {
		setAdding(false);
		if (!values) return;
		const set = Object.entries(values).filter(([, v]) => v !== "");
		const sql = set.length
			? `INSERT INTO ${ident(name)} (${set.map(([c]) => ident(c)).join(", ")}) VALUES (${set.map(() => "?").join(", ")})`
			: `INSERT INTO ${ident(name)} DEFAULT VALUES`;
		await write(sql, set.map(([, v]) => v), `Inserted a row into ${name}.`);
	}

	if (!obj) return <div className="empty"><p className="dim">{name} no longer exists.</p></div>;
	const last = Math.min(offset + PAGE, total);

	return (
		<>
			<div className="toolbar">
				<div className="seg">
					<button className={mode === "data" ? "on" : ""} onClick={() => setMode("data")}>Data</button>
					<button className={mode === "structure" ? "on" : ""} onClick={() => setMode("structure")}>Structure</button>
				</div>
				{mode === "data" && (
					<>
						<input
							className="where"
							placeholder="WHERE …   e.g. plays > 50000 AND title LIKE 'Track 1%'"
							spellCheck={false}
							value={draft}
							onChange={(e) => setDraft(e.target.value)}
							onKeyDown={(e) => {
								if (e.key === "Enter") {
									setOffset(0);
									setWhere(draft.trim());
								}
							}}
						/>
						<button title="Reload" onClick={load}>↻</button>
						<button disabled={!editable} onClick={() => setAdding(true)}>＋ Row</button>
						<button disabled={!picked.size} onClick={remove}>{picked.size ? `Delete ${picked.size}` : "Delete"}</button>
					</>
				)}
			</div>
			{mode === "data" ? (
				<>
					<div className="gridwrap">
						{error ? (
							<div className="result-meta err">{error}</div>
						) : data && (
							<>
								<Grid
									columns={shown}
									rows={body}
									types={types}
									start={offset}
									sort={sort}
									desc={desc}
									onSort={(c) => {
										setDesc(sort === c ? !desc : false);
										setSort(c);
									}}
									picked={picked}
									onPick={keyed ? (i) => {
										const p = new Set(picked);
										p.has(i) ? p.delete(i) : p.add(i);
										setPicked(p);
									} : undefined}
									onEdit={editable ? edit : undefined}
								/>
								{!body.length && <p className="dim pad">{where ? "No rows match." : "No rows."}</p>}
							</>
						)}
					</div>
					<div className="pager">
						<span>{total ? `${fmt(offset + 1)}–${fmt(last)} of ${fmt(total)}` : "0 rows"}</span>
						<span className="grow" />
						<span className="small">{editable ? "Double-click a cell to edit · click # to select" : view ? "Views are read-only" : ""}</span>
						<button disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - PAGE))}>‹ Prev</button>
						<button disabled={last >= total} onClick={() => setOffset(offset + PAGE)}>Next ›</button>
					</div>
				</>
			) : (
				<Structure name={name} obj={obj} cols={cols} schema={schema} />
			)}
			{adding && <RowDialog name={name} cols={cols} onClose={insert} />}
		</>
	);
}

function Structure({ name, obj, cols, schema }) {
	const [fks, setFks] = useState([]);
	useEffect(() => {
		rowsOf(`PRAGMA foreign_key_list(${ident(name)})`).then(setFks, () => setFks([]));
	}, [name, obj.sql]);
	const related = schema.filter((o) => o.tbl_name === name && o.name !== name);
	return (
		<div className="structure">
			<section>
				<h4>Columns</h4>
				<table className="grid">
					<thead><tr><th /><th>name</th><th>type</th><th>not null</th><th>default</th></tr></thead>
					<tbody>
						{cols.map((c) => (
							<tr key={c.cid}>
								<td>{c.pk ? <span className="tag">PK</span> : ""}</td>
								<td>{c.name}</td>
								<td className="dim">{c.type || "—"}</td>
								<td>{c.notnull ? "yes" : ""}</td>
								<td className={show(c.dflt_value).cls}>{show(c.dflt_value).text}</td>
							</tr>
						))}
					</tbody>
				</table>
			</section>
			{fks.length > 0 && (
				<section>
					<h4>Foreign keys</h4>
					<pre className="ddl">
						{fks.map((f) => `${f.from} → ${f.table}(${f.to ?? "rowid"})${f.on_delete !== "NO ACTION" ? " ON DELETE " + f.on_delete : ""}`).join("\n")}
					</pre>
				</section>
			)}
			{related.length > 0 && (
				<section>
					<h4>Indexes and triggers</h4>
					<pre className="ddl">{related.map((o) => o.sql ?? `-- ${o.name} (automatic)`).join(";\n\n")}</pre>
				</section>
			)}
			<section>
				<h4>Definition</h4>
				<pre className="ddl">{obj.sql};</pre>
			</section>
		</div>
	);
}
