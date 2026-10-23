import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ident, query, rows as rowsOf, run } from "../db.js";
import { download, fmt, show, toCSV, toJSON, typed } from "../values.js";
import { useStudio } from "../studio.js";
import Grid from "./Grid.jsx";
import { Spinner } from "./Busy.jsx";
import Chart from "./Chart.jsx";
import FilterBar, { whereOf } from "./FilterBar.jsx";
import Popover from "./Popover.jsx";
import { ArrowUpDown, Ellipsis, Filter, Maximize2, Plus, RefreshCw, Sparkles } from "lucide-react";
import RowPanel from "./RowPanel.jsx";
import ColumnPanel, { Field, Toggle } from "./ColumnPanel.jsx";
import Drawer from "./Drawer.jsx";
import { MenuButton, useContextMenu } from "./Menu.jsx";
import { DEFAULT_MODEL, MODELS, setEmbedProgress } from "../embed.js";
import { embeddingsOf, fillMissing, missingCount, search, searchVec1, setUpEmbedding } from "../vectors.js";

const PAGE = 100;

/** One table or view: its rows a page at a time, editable, and its structure. */
export default function TableView({ name, mode: initialMode = "data" }) {
	const { schema, status, changed, editTable, openQuery, importInto } = useStudio();
	const obj = schema.find((o) => o.name === name);
	const [mode, setMode] = useState(initialMode);
	const [cols, setCols] = useState([]);
	const [filt, setFilt] = useState({ filters: [], sql: "" }); // the menus' conditions, or a typed WHERE
	const [menu, openMenu] = useContextMenu();
	const [sorts, setSorts] = useState([]); // [{col, desc}]
	const [offset, setOffset] = useState(0);
	const [data, setData] = useState(null);
	const [total, setTotal] = useState(0);
	const [error, setError] = useState(null);
	const [picked, setPicked] = useState(new Set());
	const [rowPanel, setRowPanel] = useState(null); // {i} to edit row i, {} to insert
	const [colPanel, setColPanel] = useState(null); // {column} to edit, {} to add
	const [info, setInfo] = useState(""); // the last read's count and time
	// Semantic search: the text searched for, and how.
	const [embs, setEmbs] = useState([]);
	const [embIdx, setEmbIdx] = useState(0);
	const [missing, setMissing] = useState(0);
	const [semDraft, setSemDraft] = useState("");
	const [semantic, setSemantic] = useState(null); // {text, keywords}
	const [keywords, setKeywords] = useState(true);
	const [vec1Model, setVec1Model] = useState(DEFAULT_MODEL);
	const [embedding, setEmbedding] = useState(false);
	const filling = useRef(false);

	useEffect(() => setMode(initialMode), [initialMode]);

	const sql = obj?.sql ?? "";
	const view = obj?.type === "view";
	const virtual = /^\s*CREATE\s+VIRTUAL\b/i.test(sql);
	const vec1 = virtual && /\bUSING\s+vec1\b/i.test(sql);
	const withoutRowid = /\bWITHOUT\s+ROWID\b/i.test(sql);
	const pk = useMemo(() => cols.filter((c) => c.pk > 0).sort((a, b) => a.pk - b.pk).map((c) => c.name), [cols]);
	// A row is named by its rowid, or by its primary key in a WITHOUT ROWID
	// table. A view's rows have no name, so a view is read-only.
	const keyCols = withoutRowid ? pk : ["rowid"];
	const keyed = !view && keyCols.length > 0;
	const editable = keyed && !virtual;
	const emb = embs[embIdx] ?? null;
	const built = useMemo(() => whereOf(filt.filters, Object.fromEntries(cols.map((c) => [c.name, c.type]))), [filt, cols]);
	const where = filt.sql || built.sql;
	const whereParams = filt.sql ? [] : built.params;
	const orderBy = sorts.length ? ` ORDER BY ${sorts.map((x) => `${ident(x.col)}${x.desc ? " DESC" : ""}`).join(", ")}` : "";

	const loadCols = useCallback(async () => {
		try {
			const c = await rowsOf(`PRAGMA table_info(${ident(name)})`);
			setCols(c);
			const e = view || virtual ? [] : await embeddingsOf(name, c);
			setEmbs(e);
			setEmbIdx((i) => Math.min(i, Math.max(0, e.length - 1)));
		} catch (e) {
			status(e.message, true);
		}
	}, [name, sql]);
	useEffect(() => {
		loadCols();
	}, [loadCols]);

	const textCols = cols.filter((c) => /CHAR|CLOB|TEXT/i.test(c.type) || c.type === "").map((c) => c.name);

	useEffect(() => {
		setEmbedProgress((p) => p.total && status(`Downloading ${p.model.split("/").pop()}… ${Math.round((100 * p.loaded) / p.total)}%`));
	}, [status]);

	const refreshMissing = useCallback(async () => {
		setMissing(emb?.src ? await missingCount(name, emb).catch(() => 0) : 0);
	}, [name, emb?.vec, emb?.src]);
	useEffect(() => {
		refreshMissing();
	}, [refreshMissing]);

	const load = useCallback(async () => {
		if (semantic) {
			try {
				const t = performance.now();
				const r = vec1 ? await searchVec1(name, vec1Model, semantic.text) : await search(name, emb, semantic.text, semantic.keywords);
				setData({ ...r, semantic: true });
				setTotal(r.rows.length);
				setError(null);
				setPicked(new Set());
				setInfo(`closest to “${semantic.text}” by ${r.how} · ${(performance.now() - t).toFixed(0)} ms`);
			} catch (e) {
				setError(e.message);
				status(e.message, true);
			}
			return;
		}
		const filter = where ? ` WHERE ${where}` : "";
		const fp = whereParams;
		const order = orderBy;
		const key = keyed ? keyCols.map((k, i) => `${ident(k)} AS "__k${i}"`).join(", ") + ", " : "";
		try {
			const t = performance.now();
			const n = (await query(`SELECT count(*) FROM ${ident(name)}${filter}`, fp)).rows[0][0];
			const r = await query(`SELECT ${key}* FROM ${ident(name)}${filter}${order} LIMIT ${PAGE} OFFSET ${offset}`, fp);
			setTotal(Number(n));
			setData(r);
			setError(null);
			setPicked(new Set());
			setInfo(`${(performance.now() - t).toFixed(1)} ms`);
		} catch (e) {
			setError(e.message);
			status(e.message, true);
		}
	}, [name, where, JSON.stringify(whereParams), orderBy, offset, keyed, keyCols.join("\0"), semantic, emb, vec1, vec1Model]);

	useEffect(() => {
		if (cols.length || view) load();
	}, [load, cols]);
	// The latest load, for work that finishes after a render (fill).
	const loadRef = useRef(load);
	loadRef.current = load;

	// fill embeds the rows still missing a vector, in the background of the
	// page: after "Embed…", and after every write the studio makes here.
	async function fill(e = emb) {
		if (!e?.src || !e.model || filling.current) return;
		filling.current = true;
		try {
			const t = performance.now();
			const n = await fillMissing(name, e, (done, all) => status(`Embedding ${e.src}: ${fmt(done)} of ${fmt(all)}…`));
			if (n) status(`Embedded ${fmt(n)} row${n === 1 ? "" : "s"} of ${e.src} with ${MODELS[e.model].name} in ${((performance.now() - t) / 1000).toFixed(1)} s.`);
			if (n) {
				changed(name);
				loadRef.current();
			}
		} catch (err) {
			status(`Embedding failed: ${err.message}`, true);
		} finally {
			filling.current = false;
			refreshMissing();
		}
	}

	// Search results carry no row keys, so they are read-only.
	const searching = !!data?.semantic;
	const nk = keyed && !searching ? keyCols.length : 0;
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
			fill();
		} catch (e) {
			status(e.message, true);
		}
	}

	async function edit(i, col, text) {
		if (body[i][shown.indexOf(col)] instanceof Uint8Array) {
			status("BLOB values are edited with SQL, in a query tab.", true);
			return;
		}
		await write(`UPDATE ${ident(name)} SET ${ident(col)} = ? WHERE ${keyWhere}`, [typed(text, types[col]), ...keyOf(i)], `Updated ${col}.`);
	}

	async function remove() {
		const list = [...picked];
		if (!confirm(`Delete ${list.length} row${list.length > 1 ? "s" : ""} from ${name}?`)) return;
		try {
			await run("BEGIN");
			for (const i of list) await run(`DELETE FROM ${ident(name)} WHERE ${keyWhere}`, keyOf(i));
			await run("COMMIT");
			status(`Deleted ${list.length} row${list.length > 1 ? "s" : ""}.`);
		} catch (e) {
			await run("ROLLBACK").catch(() => {});
			status(e.message, true);
		}
		changed(name);
		await load();
	}

	async function embedColumn(opts) {
		setEmbedding(false);
		if (!opts) return;
		try {
			status(`Setting up ${opts.col}_emb…`);
			const e = await setUpEmbedding(name, opts.col, opts.model, opts);
			changed(name);
			const list = await embeddingsOf(name, await rowsOf(`PRAGMA table_info(${ident(name)})`));
			setEmbs(list);
			setEmbIdx(Math.max(0, list.findIndex((x) => x.vec === e.vec)));
			await fill(e);
		} catch (err) {
			status(err.message, true);
		}
	}

	// saveRow writes a row panel's fields: an INSERT, or an UPDATE of row i.
	// It reports whether the write went through, so the panel can stay open.
	async function saveRow(i, values) {
		const set = Object.entries(values);
		const val = ([c, v]) => (v === null ? null : typed(v, types[c]));
		let sql;
		let params;
		if (i == null) {
			sql = set.length
				? `INSERT INTO ${ident(name)} (${set.map(([c]) => ident(c)).join(", ")}) VALUES (${set.map(() => "?").join(", ")})`
				: `INSERT INTO ${ident(name)} DEFAULT VALUES`;
			params = set.map(val);
		} else {
			sql = `UPDATE ${ident(name)} SET ${set.map(([c]) => `${ident(c)} = ?`).join(", ")} WHERE ${keyWhere}`;
			params = [...set.map(val), ...keyOf(i)];
		}
		try {
			await run(sql, params);
			status(i == null ? `Inserted a row into ${name}.` : "Saved the row.");
			setRowPanel(null);
			changed(name);
			await load();
			fill();
			return true;
		} catch (e) {
			status(e.message, true);
			return false;
		}
	}

	// exportRows downloads every row the view shows (the filter and order,
	// not just this page) as CSV or JSON.
	async function exportRows(kind) {
		try {
			const r = searching
				? data
				: await query(`SELECT * FROM ${ident(name)}${where ? ` WHERE ${where}` : ""}${orderBy}`, whereParams);
			const cs = searching ? shown : r.columns;
			const rs = searching ? body : r.rows;
			download(`${name}.${kind}`, kind === "csv" ? toCSV(cs, rs) : toJSON(cs, rs), kind === "csv" ? "text/csv" : "application/json");
			status(`Exported ${fmt(rs.length)} rows of ${name} as ${kind.toUpperCase()}.`);
		} catch (e) {
			status(e.message, true);
		}
	}

	const canSearch = vec1 || (emb && emb.model);
	const runSearch = (text) => setSemantic(text.trim() ? { text: text.trim(), keywords } : null);

	function togglePick(i) {
		const p = new Set(picked);
		p.has(i) ? p.delete(i) : p.add(i);
		setPicked(p);
	}

	// cellMenu is a right-click on a cell: edit it, copy it, filter by it, or
	// select or delete its row.
	function cellMenu(i, col, startEdit) {
		const v = body[i][shown.indexOf(col)];
		const text = v === null ? null : v instanceof Uint8Array ? null : String(v);
		const by = (op) => setFilt({ filters: [...filt.filters, { col, op, value: text ?? "" }], sql: "" });
		return [
			{ label: editable && !searching ? "Edit row…" : "View row…", hint: "Space", onClick: () => setRowPanel({ i }) },
			editable && !searching && { label: "Edit cell", hint: "Enter", onClick: startEdit },
			editable && !searching && v !== null && { label: "Set to NULL", onClick: () => write(`UPDATE ${ident(name)} SET ${ident(col)} = NULL WHERE ${keyWhere}`, keyOf(i), `Set ${col} to NULL.`) },
			{ label: "Copy value", hint: "⌘C", onClick: () => navigator.clipboard?.writeText(text ?? "NULL") },
			{ label: "Copy row as JSON", onClick: () => navigator.clipboard?.writeText(JSON.stringify(Object.fromEntries(shown.map((c, j) => [c, typeof body[i][j] === "bigint" ? String(body[i][j]) : body[i][j] instanceof Uint8Array ? null : body[i][j]])))) },
			"-",
			!searching && cols.some((c) => c.name === col) && (v === null
				? { label: `Filter: ${col} is empty`, onClick: () => by("null") }
				: text !== null && { label: `Filter: ${col} = ${text.length > 24 ? text.slice(0, 24) + "…" : text}`, onClick: () => by("=") }),
			!searching && text !== null && cols.some((c) => c.name === col) && { label: `Filter: ${col} ≠ this`, onClick: () => by("!=") },
			keyed && !searching && "-",
			keyed && !searching && { label: picked.has(i) ? "Unselect row" : "Select row", onClick: () => togglePick(i) },
			editable && !searching && { label: "Delete row", danger: true, onClick: () => confirm(`Delete this row from ${name}?`) && write(`DELETE FROM ${ident(name)} WHERE ${keyWhere}`, keyOf(i), "Deleted a row.") },
		];
	}

	if (!obj) return <div className="empty"><p className="dim">{name} no longer exists.</p></div>;
	const last = Math.min(offset + PAGE, total);
	const tables = schema.filter((o) => o.type === "table").map((o) => o.name);
	const nf = filt.sql ? 1 : filt.filters.length;
	const sortBy = (col, d) => setSorts([{ col, desc: d }]);

	// headerMenu is a click on a column's ▾: sort by it, filter on it, or change it.
	const headerMenu = (e, col) =>
		openMenu(e, [
			{ label: "Sort ascending", hint: "A → Z", onClick: () => sortBy(col, false) },
			{ label: "Sort descending", hint: "Z → A", onClick: () => sortBy(col, true) },
			sorts.length > 0 && { label: "Clear sort", onClick: () => setSorts([]) },
			"-",
			{ label: `Filter on ${col}…`, onClick: () => setFilt({ filters: [...filt.filters, { col, op: "=", value: "" }], sql: "" }) },
			editable && "-",
			editable && { label: "Edit column…", onClick: () => setColPanel({ column: col }) },
			editable && { label: "Delete column", danger: true, onClick: () => setColPanel({ column: col }) },
		]);

	return (
		<div className="tablepage">
			{mode === "data" && (
				<div className="toolbar tbar">
					{!searching && (
						<>
							<Popover className={`ghost chipbtn${nf ? " active" : ""}`} label={<><Filter size={14} />{nf ? `Filtered by ${nf}` : "Filter"}</>} width={560}>
								<div className="popover-head">
									<strong>Filter rows</strong>
									<span className="dim small">conditions are ANDed</span>
								</div>
								<FilterBar key={name} columns={cols.map((c) => c.name)} filters={filt.filters} sql={filt.sql} onApply={(f) => { setOffset(0); setFilt({ filters: f.filters ?? [], sql: f.sql ?? "" }); }} />
							</Popover>
							<Popover className={`ghost chipbtn${sorts.length ? " active" : ""}`} label={<><ArrowUpDown size={14} />{sorts.length ? `Sorted by ${sorts.length}` : "Sort"}</>} width={380}>
								<div className="popover-head"><strong>Sort rows</strong></div>
								<div className="sortlist">
									{sorts.map((x, k) => (
										<div key={k} className="sortrow">
											<select value={x.col} onChange={(e) => setSorts(sorts.map((y, m) => (m === k ? { ...y, col: e.target.value } : y)))}>
												{cols.map((c) => <option key={c.name}>{c.name}</option>)}
											</select>
											<div className="seg">
												<button className={!x.desc ? "on" : ""} onClick={() => setSorts(sorts.map((y, m) => (m === k ? { ...y, desc: false } : y)))}>ascending</button>
												<button className={x.desc ? "on" : ""} onClick={() => setSorts(sorts.map((y, m) => (m === k ? { ...y, desc: true } : y)))}>descending</button>
											</div>
											<button className="link" onClick={() => setSorts(sorts.filter((_, m) => m !== k))}>×</button>
										</div>
									))}
									{!sorts.length && <p className="dim small">Not sorted: rows come in storage order.</p>}
									<button className="link" onClick={() => setSorts([...sorts, { col: cols.find((c) => !sorts.some((x) => x.col === c.name))?.name ?? cols[0]?.name, desc: false }])}>＋ Add a sort</button>
								</div>
							</Popover>
						</>
					)}
					{canSearch && (
						<div className="searchbox">
							<Sparkles size={14} />
							<input
								placeholder={`Search ${vec1 ? name : emb.src ?? emb.vec} by meaning…`}
								spellCheck={false}
								value={semDraft}
								onChange={(e) => setSemDraft(e.target.value)}
								onKeyDown={(e) => {
									if (e.key === "Enter") runSearch(semDraft);
									if (e.key === "Escape") { setSemDraft(""); setSemantic(null); }
								}}
							/>
							{semantic && <button className="link" onClick={() => { setSemDraft(""); setSemantic(null); }}>×</button>}
						</div>
					)}
					{editable && (
						<MenuButton
							className="primary"
							label={<><Plus size={14} />Insert</>}
							items={[
								{ label: "Insert a row", hint: "a form", onClick: () => setRowPanel({}) },
								{ label: "Insert a column", onClick: () => setColPanel({}) },
								"-",
								{ label: "Import rows from CSV or JSON…", onClick: () => importInto(name) },
							]}
						/>
					)}
					<span className="grow" />
					{missing > 0 && emb?.model && (
						<button className="warn" title={`${fmt(missing)} rows of ${emb.src} have no vector yet (new, or their text changed)`} onClick={() => fill()}>
							✦ Embed {fmt(missing)} new
						</button>
					)}
					{picked.size > 0 && (
						<span className="selbar">
							{picked.size} row{picked.size > 1 ? "s" : ""} selected
							<button className="danger" onClick={remove}>Delete</button>
							<button className="link" onClick={() => setPicked(new Set())}>clear</button>
						</span>
					)}
					<button className="ghost icon" title="Reload" onClick={load}><RefreshCw size={14} /></button>
					<MenuButton
						className="ghost icon"
						label={<Ellipsis size={16} />}
						title="More"
						items={[
							{ label: "Query in SQL editor", onClick: () => openQuery(`SELECT * FROM ${ident(name)}${where && !whereParams.length ? ` WHERE ${where}` : ""}${orderBy}\nLIMIT 100;`, name) },
							editable && !withoutRowid && textCols.length > 0 && { label: "✦ Make searchable by meaning…", onClick: () => setEmbedding(true) },
							"-",
							{ label: "Download CSV", hint: nf ? "filtered rows" : "all rows", onClick: () => exportRows("csv") },
							{ label: "Download JSON", hint: nf ? "filtered rows" : "all rows", onClick: () => exportRows("json") },
						]}
					/>
				</div>
			)}

			<div className="gridwrap">
				{mode === "structure" ? (
					<Structure name={name} obj={obj} cols={cols} schema={schema} editable={editable} onColumn={(column) => setColPanel(column ? { column } : {})} onChanged={() => { changed(name); loadCols(); }} />
				) : error ? (
					<div className="result-meta err">{error}</div>
				) : !data ? (
					<Spinner label={`Reading ${name}…`} />
				) : mode === "chart" ? (
					<Chart key={name + offset} columns={shown} rows={body} />
				) : (
					<>
						<Grid
							columns={shown}
							rows={body}
							types={types}
							start={searching ? 0 : offset}
							sort={sorts.length === 1 ? sorts[0].col : null}
							desc={sorts[0]?.desc}
							onSort={searching ? undefined : (c) => sortBy(c, sorts.length === 1 && sorts[0].col === c ? !sorts[0].desc : false)}
							onHeaderMenu={searching ? undefined : headerMenu}
							picked={picked}
							onPick={keyed && !searching ? togglePick : undefined}
							onOpen={(i) => setRowPanel({ i })}
							onEdit={editable && !searching ? edit : undefined}
							onContextMenu={(e, i, col, startEdit) => openMenu(e, cellMenu(i, col, startEdit))}
						/>
						{!body.length && (
							<div className="emptyrows">
								<div className="emptyicon">{where ? "⏷" : "▦"}</div>
								<p>{where ? "No rows match the filter." : `${name} has no rows yet.`}</p>
								{where ? <button onClick={() => setFilt({ filters: [], sql: "" })}>Clear the filter</button> : editable && <button className="primary" onClick={() => setRowPanel({})}>Insert a row</button>}
							</div>
						)}
					</>
				)}
			</div>

			<div className="tfoot">
				<div className="seg">
					<button className={mode === "data" ? "on" : ""} onClick={() => setMode("data")}>Data</button>
					<button className={mode === "chart" ? "on" : ""} onClick={() => setMode("chart")}>Chart</button>
					<button className={mode === "structure" ? "on" : ""} onClick={() => setMode("structure")}>Definition</button>
				</div>
				<span className="grow" />
				{mode !== "structure" && (
					<>
						<span className="dim small">{searching ? info : total ? `${fmt(offset + 1)}–${fmt(last)} of ${fmt(total)} ${where ? "matching " : ""}rows · ${info}` : "0 rows"}</span>
						{!searching && (
							<div className="pagebtns">
								<button className="ghost icon" title="Previous page" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - PAGE))}>‹</button>
								<button className="ghost icon" title="Next page" disabled={last >= total} onClick={() => setOffset(offset + PAGE)}>›</button>
							</div>
						)}
					</>
				)}
				{mode === "structure" && editable && <button className="primary" onClick={() => editTable(name)}>Edit table</button>}
			</div>
			{menu}
			{rowPanel && (
				<RowPanel
					name={name}
					cols={cols}
					readOnly={!editable || searching}
					row={rowPanel.i == null ? null : Object.fromEntries(shown.map((c, j) => [c, body[rowPanel.i][j]]))}
					onSave={(v) => saveRow(rowPanel.i, v)}
					onClose={() => setRowPanel(null)}
				/>
			)}
			{colPanel && (
				<ColumnPanel
					table={name}
					column={colPanel.column ?? null}
					tables={tables}
					status={status}
					onClose={(done) => {
						setColPanel(null);
						if (done) {
							status(colPanel.column ? "Saved the column." : "Added the column.");
							changed(name);
							loadCols();
						}
					}}
				/>
			)}
			{embedding && <EmbedDialog name={name} textCols={textCols} embs={embs} onClose={embedColumn} />}
		</div>
	);
}

// EmbedDialog asks which text column to embed, with which model, and which
// indexes to build; onClose({col, model, index, keywords}) or onClose(null).
function EmbedDialog({ name, textCols, embs, onClose }) {
	const [col, setCol] = useState(textCols[0]);
	const had = embs.find((e) => e.src === col);
	const [model, setModel] = useState(had?.model ?? DEFAULT_MODEL);
	const [index, setIndex] = useState(false);
	const [keywords, setKeywords] = useState(true);
	const ref = useRef(null);
	useEffect(() => ref.current.showModal(), []);
	const m = MODELS[model];
	return (
		<dialog ref={ref} onClose={() => onClose(null)}>
			<form onSubmit={(e) => { e.preventDefault(); onClose({ col, model, index, keywords }); }}>
				<h2>Embed a column of {name}</h2>
				<p className="dim small">
					Each row's text becomes a {m.dims}-dimension vector, computed in your browser, in the column{" "}
					<code>{col}_emb F32_BLOB({m.dims})</code>. A row whose text changes, or a new row, gets its vector the next time the
					studio embeds (it offers to). The model downloads once.
				</p>
				<div className="fields">
					<label className="field">
						<span>column</span>
						<select value={col} onChange={(e) => setCol(e.target.value)}>
							{textCols.map((c) => <option key={c}>{c}</option>)}
						</select>
					</label>
					<label className="field">
						<span>model</span>
						<select value={model} onChange={(e) => setModel(e.target.value)}>
							{Object.entries(MODELS).map(([k, x]) => <option key={k} value={k}>{x.name} — {x.about}</option>)}
						</select>
					</label>
					{had && had.model !== model && <p className="warn small">{col}_emb was made with {MODELS[had.model]?.name ?? had.model}: every row is embedded again.</p>}
					<label className="check">
						<input type="checkbox" checked={keywords} onChange={(e) => setKeywords(e.target.checked)} />
						Also a full-text index (fts5), so a search can rank by keywords and meaning together
					</label>
					<label className="check">
						<input type="checkbox" checked={index} onChange={(e) => setIndex(e.target.checked)} />
						A vector index (libsql_vector_idx): exact up to 100,000 rows, then approximate and faster
					</label>
				</div>
				<div className="dialog-actions">
					<button type="button" onClick={() => onClose(null)}>Cancel</button>
					<button type="submit" className="primary">Embed</button>
				</div>
			</form>
		</dialog>
	);
}

// Structure shows a table's columns, keys, indexes and DDL, and changes it:
// add, rename and drop columns, add an index, rename or drop the table.
function Structure({ name, obj, cols, schema, editable, onColumn, onChanged }) {
	const { status, openQuery } = useStudio();
	const [fks, setFks] = useState([]);
	const [newIdx, setNewIdx] = useState({ cols: [], unique: false, name: "" });
	const [idxOpen, setIdxOpen] = useState(false);
	useEffect(() => {
		rowsOf(`PRAGMA foreign_key_list(${ident(name)})`).then(setFks, () => setFks([]));
	}, [name, obj.sql]);
	const related = schema.filter((o) => o.tbl_name === name && o.name !== name);
	const table = obj.type === "table";

	async function change(sql, msg) {
		try {
			await run(sql);
			status(msg);
			onChanged();
			return true;
		} catch (e) {
			status(e.message, true);
			return false;
		}
	}

	return (
		<div className="structure">
			<section>
				<div className="sechead">
					<h4>Columns</h4>
					{editable && <span className="dim small">Click a column to change it</span>}
				</div>
				<table className={`grid${editable ? " clickrows" : ""}`}>
					<thead><tr><th /><th>name</th><th>type</th><th>not null</th><th>default</th><th>references</th></tr></thead>
					<tbody>
						{cols.map((c) => {
							const fk = fks.find((f) => f.from === c.name);
							return (
								<tr key={c.cid} onClick={editable ? () => onColumn(c.name) : undefined} title={editable ? `Edit ${c.name}` : undefined}>
									<td>{c.pk ? <span className="tag">PK</span> : ""}</td>
									<td>{c.name}</td>
									<td className="dim">{c.type || "—"}</td>
									<td>{c.notnull ? "yes" : ""}</td>
									<td className={show(c.dflt_value).cls}>{show(c.dflt_value).text}</td>
									<td className="dim">{fk ? `${fk.table}(${fk.to ?? "rowid"})${fk.on_delete !== "NO ACTION" ? " · on delete " + fk.on_delete.toLowerCase() : ""}` : ""}</td>
								</tr>
							);
						})}
					</tbody>
				</table>
				{editable && <button className="addcol" onClick={() => onColumn(null)}>＋ Add a column</button>}
			</section>
			<section>
				<h4>Indexes and triggers</h4>
				{!related.length && <p className="dim small">No indexes or triggers. An index speeds up filters and joins on its columns.</p>}
				{related.length > 0 && <pre className="ddl">{related.map((o) => o.sql ?? `-- ${o.name} (automatic)`).join(";\n\n")}</pre>}
				{related.filter((o) => o.sql && (o.type === "index" || o.type === "trigger")).map((o) => (
					<div key={o.name} className="inline objrow">
						<span className="mono small">{o.type} {o.name}</span>
						<button className="link" title="Open its definition in a query tab, to change and run" onClick={() => openQuery(`DROP ${o.type.toUpperCase()} ${ident(o.name)};\n${o.sql};\n`, `Edit ${o.name}`)}>edit</button>
						<button className="link" onClick={() => confirm(`Drop ${o.type} ${o.name}?`) && change(`DROP ${o.type.toUpperCase()} ${ident(o.name)}`, `Dropped ${o.name}.`)}>drop</button>
					</div>
				))}
				<div className="inline">
					{editable && <button onClick={() => setIdxOpen(true)}>＋ Create index</button>}
					{table && <button className="ghost" onClick={() => openQuery(`CREATE TRIGGER ${ident(name + "_trigger")} AFTER INSERT ON ${ident(name)}\nBEGIN\n  SELECT 1; -- the trigger's statements; NEW is the row\nEND;\n`, "New trigger")}>＋ Trigger in SQL</button>}
				</div>
				{idxOpen && (
					<Drawer
						title="Create an index"
						subtitle={`on ${name} · speeds up filters, sorts and joins on its columns`}
						onClose={() => setIdxOpen(false)}
						footer={
							<>
								<span className="grow" />
								<button onClick={() => setIdxOpen(false)}>Cancel</button>
								<button className="primary" disabled={!newIdx.cols.length} onClick={async () => {
									const idx = newIdx.name.trim() || `${name}_${newIdx.cols.join("_")}_idx`;
									if (await change(`CREATE ${newIdx.unique ? "UNIQUE " : ""}INDEX ${ident(idx)} ON ${ident(name)}(${newIdx.cols.map(ident).join(", ")})`, `Created ${idx}.`)) {
										setNewIdx({ cols: [], unique: false, name: "" });
										setIdxOpen(false);
									}
								}}>Create</button>
							</>
						}
					>
						<div className="form">
							<div className="ffield">
								<span className="flabel2">Columns</span>
								<span className="fhelp">In the order they are picked; the first matters most.</span>
								<div className="pickcols">
									{cols.map((c) => {
										const k = newIdx.cols.indexOf(c.name);
										return (
											<label key={c.name} className={`pickcol${k >= 0 ? " on" : ""}`}>
												<input type="checkbox" checked={k >= 0} onChange={(e) => setNewIdx({ ...newIdx, cols: e.target.checked ? [...newIdx.cols, c.name] : newIdx.cols.filter((x) => x !== c.name) })} />
												<span className="cname">{c.name}</span>
												<span className="badge">{(c.type || "any").toLowerCase()}</span>
												{k >= 0 && <span className="order">{k + 1}</span>}
											</label>
										);
									})}
								</div>
							</div>
							<Field label="Name" help="Leave empty for a name made from the columns">
								<input spellCheck={false} placeholder={newIdx.cols.length ? `${name}_${newIdx.cols.join("_")}_idx` : `${name}_…_idx`} value={newIdx.name} onChange={(e) => setNewIdx({ ...newIdx, name: e.target.value })} />
							</Field>
							<div className="toggles">
								<Toggle label="Unique" help="Refuse two rows with the same values in these columns" checked={newIdx.unique} onChange={(v) => setNewIdx({ ...newIdx, unique: v })} />
							</div>
						</div>
					</Drawer>
				)}
			</section>
			<section>
				<h4>Definition</h4>
				<pre className="ddl">{obj.sql};</pre>
				{(table || obj.type === "view") && (
					<div className="inline">
						{obj.type === "view" && <button onClick={() => openQuery(`DROP VIEW ${ident(name)};\n${obj.sql};\n`, `Edit ${name}`)}>Edit as SQL…</button>}
						<button className="danger" onClick={() => confirm(`Drop ${obj.type} ${name}? This cannot be undone.`) && change(`DROP ${obj.type.toUpperCase()} ${ident(name)}`, `Dropped ${name}.`)}>Drop {obj.type}</button>
					</div>
				)}
			</section>
		</div>
	);
}

/** A column type: the common ones to pick, or any typed in. */
export function TypeInput({ value, onChange }) {
	return (
		<>
			<input list="coltypes" className="type" spellCheck={false} value={value} onChange={(e) => onChange(e.target.value)} />
			<datalist id="coltypes">
				{["INTEGER", "TEXT", "REAL", "BLOB", "NUMERIC", "F32_BLOB(384)", "DATETIME", "BOOLEAN", "JSON"].map((t) => <option key={t} value={t} />)}
			</datalist>
		</>
	);
}
