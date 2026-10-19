import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ident, query, rows as rowsOf, run } from "../db.js";
import { download, fmt, show, toCSV, toJSON, typed } from "../values.js";
import { useStudio } from "../studio.js";
import Grid from "./Grid.jsx";
import RowDialog from "./RowDialog.jsx";
import { DEFAULT_MODEL, MODELS, setEmbedProgress } from "../embed.js";
import { embeddingsOf, fillMissing, missingCount, search, searchVec1, setUpEmbedding } from "../vectors.js";

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
				status(`${name}: ${r.rows.length} closest to “${semantic.text}” by ${r.how} · ${(performance.now() - t).toFixed(0)} ms`);
			} catch (e) {
				setError(e.message);
				status(e.message, true);
			}
			return;
		}
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
	}, [name, where, sort, desc, offset, keyed, keyCols.join("\0"), semantic, emb, vec1, vec1Model]);

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

	async function insert(values) {
		setAdding(false);
		if (!values) return;
		const set = Object.entries(values).filter(([, v]) => v !== "");
		const sql = set.length
			? `INSERT INTO ${ident(name)} (${set.map(([c]) => ident(c)).join(", ")}) VALUES (${set.map(() => "?").join(", ")})`
			: `INSERT INTO ${ident(name)} DEFAULT VALUES`;
		await write(sql, set.map(([c, v]) => typed(v, types[c])), `Inserted a row into ${name}.`);
	}

	// exportRows downloads every row the view shows (the filter and order,
	// not just this page) as CSV or JSON.
	async function exportRows(kind) {
		try {
			const r = searching
				? data
				: await query(`SELECT * FROM ${ident(name)}${where ? ` WHERE ${where}` : ""}${sort ? ` ORDER BY ${ident(sort)}${desc ? " DESC" : ""}` : ""}`);
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
						{canSearch && (
							<>
								{embs.length > 1 && (
									<select title="The vector column searched" value={embIdx} onChange={(e) => { setEmbIdx(Number(e.target.value)); setSemantic(null); }}>
										{embs.map((e, i) => <option key={e.vec} value={i}>{e.vec}</option>)}
									</select>
								)}
								{vec1 && (
									<select title="The model a search's text is embedded with: the one the table's vectors came from" value={vec1Model} onChange={(e) => setVec1Model(e.target.value)}>
										{Object.entries(MODELS).map(([k, m]) => <option key={k} value={k}>{m.name}</option>)}
									</select>
								)}
								<input
									className="where semantic"
									placeholder={`✦ Search ${vec1 ? name : emb.src ?? emb.vec} by meaning…`}
									spellCheck={false}
									value={semDraft}
									onChange={(e) => setSemDraft(e.target.value)}
									onKeyDown={(e) => {
										if (e.key === "Enter") runSearch(semDraft);
										if (e.key === "Escape") { setSemDraft(""); setSemantic(null); }
									}}
								/>
								{emb?.fts && (
									<label className="check" title="Rank by the words too, through the full-text index">
										<input type="checkbox" checked={keywords} onChange={(e) => { setKeywords(e.target.checked); if (semantic) setSemantic({ ...semantic, keywords: e.target.checked }); }} />
										keywords
									</label>
								)}
							</>
						)}
						{semantic && <button title="Back to the table" onClick={() => { setSemDraft(""); setSemantic(null); }}>✕ Search</button>}
						{missing > 0 && emb?.model && (
							<button className="warn" title={`${fmt(missing)} rows of ${emb.src} have no vector yet (new, or their text changed)`} onClick={() => fill()}>
								✦ Embed {fmt(missing)} new
							</button>
						)}
						<button title="Reload" onClick={load}>↻</button>
						<button disabled={!editable || withoutRowid || !textCols.length} title="Embed a text column for semantic search" onClick={() => setEmbedding(true)}>✦ Embed…</button>
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
									start={searching ? 0 : offset}
									sort={sort}
									desc={desc}
									onSort={searching ? undefined : (c) => {
										setDesc(sort === c ? !desc : false);
										setSort(c);
									}}
									picked={picked}
									onPick={keyed && !searching ? (i) => {
										const p = new Set(picked);
										p.has(i) ? p.delete(i) : p.add(i);
										setPicked(p);
									} : undefined}
									onEdit={editable && !searching ? edit : undefined}
								/>
								{!body.length && <p className="dim pad">{where ? "No rows match." : "No rows."}</p>}
							</>
						)}
					</div>
					<div className="pager">
						<span>{searching ? `${fmt(total)} closest` : total ? `${fmt(offset + 1)}–${fmt(last)} of ${fmt(total)}` : "0 rows"}</span>
						<span className="grow" />
						<span className="small">{searching ? "Closest first" : editable ? "Double-click a cell to edit · click # to select" : view ? "Views are read-only" : ""}</span>
						<button title="Download the rows shown (all pages) as CSV" onClick={() => exportRows("csv")}>CSV</button>
						<button title="Download the rows shown (all pages) as JSON" onClick={() => exportRows("json")}>JSON</button>
						{!searching && <button disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - PAGE))}>‹ Prev</button>}
						{!searching && <button disabled={last >= total} onClick={() => setOffset(offset + PAGE)}>Next ›</button>}
					</div>
				</>
			) : (
				<Structure name={name} obj={obj} cols={cols} schema={schema} editable={editable} onChanged={() => { changed(name); loadCols(); }} />
			)}
			{adding && <RowDialog name={name} cols={cols} onClose={insert} />}
			{embedding && <EmbedDialog name={name} textCols={textCols} embs={embs} onClose={embedColumn} />}
		</>
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
function Structure({ name, obj, cols, schema, editable, onChanged }) {
	const { status } = useStudio();
	const [fks, setFks] = useState([]);
	const [newCol, setNewCol] = useState({ name: "", type: "TEXT" });
	const [newIdx, setNewIdx] = useState({ cols: [], unique: false });
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
	const renameCol = (c) => {
		const to = prompt(`Rename column ${c} to:`, c);
		if (to && to !== c) change(`ALTER TABLE ${ident(name)} RENAME COLUMN ${ident(c)} TO ${ident(to)}`, `Renamed ${c} to ${to}.`);
	};
	const dropCol = (c) => confirm(`Drop column ${c} of ${name}? Its values are lost.`) && change(`ALTER TABLE ${ident(name)} DROP COLUMN ${ident(c)}`, `Dropped ${c}.`);

	return (
		<div className="structure">
			<section>
				<h4>Columns</h4>
				<table className="grid">
					<thead><tr><th /><th>name</th><th>type</th><th>not null</th><th>default</th>{editable && <th />}</tr></thead>
					<tbody>
						{cols.map((c) => (
							<tr key={c.cid}>
								<td>{c.pk ? <span className="tag">PK</span> : ""}</td>
								<td>{c.name}</td>
								<td className="dim">{c.type || "—"}</td>
								<td>{c.notnull ? "yes" : ""}</td>
								<td className={show(c.dflt_value).cls}>{show(c.dflt_value).text}</td>
								{editable && (
									<td className="rowactions">
										<button className="link" onClick={() => renameCol(c.name)}>rename</button>
										{!c.pk && <button className="link" onClick={() => dropCol(c.name)}>drop</button>}
									</td>
								)}
							</tr>
						))}
					</tbody>
				</table>
				{editable && (
					<form className="inline" onSubmit={async (e) => {
						e.preventDefault();
						if (newCol.name && await change(`ALTER TABLE ${ident(name)} ADD COLUMN ${ident(newCol.name)} ${newCol.type}`, `Added ${newCol.name}.`)) setNewCol({ name: "", type: newCol.type });
					}}>
						<input placeholder="new column" spellCheck={false} value={newCol.name} onChange={(e) => setNewCol({ ...newCol, name: e.target.value })} />
						<TypeInput value={newCol.type} onChange={(type) => setNewCol({ ...newCol, type })} />
						<button type="submit" disabled={!newCol.name}>＋ Column</button>
					</form>
				)}
			</section>
			{fks.length > 0 && (
				<section>
					<h4>Foreign keys</h4>
					<pre className="ddl">
						{fks.map((f) => `${f.from} → ${f.table}(${f.to ?? "rowid"})${f.on_delete !== "NO ACTION" ? " ON DELETE " + f.on_delete : ""}`).join("\n")}
					</pre>
				</section>
			)}
			<section>
				<h4>Indexes and triggers</h4>
				{related.length > 0 && <pre className="ddl">{related.map((o) => o.sql ?? `-- ${o.name} (automatic)`).join(";\n\n")}</pre>}
				{related.filter((o) => o.sql && (o.type === "index" || o.type === "trigger")).map((o) => (
					<button key={o.name} className="link" onClick={() => confirm(`Drop ${o.type} ${o.name}?`) && change(`DROP ${o.type.toUpperCase()} ${ident(o.name)}`, `Dropped ${o.name}.`)}>
						drop {o.name}
					</button>
				))}
				{editable && (
					<form className="inline" onSubmit={async (e) => {
						e.preventDefault();
						if (!newIdx.cols.length) return;
						const idx = `${name}_${newIdx.cols.join("_")}_idx`;
						if (await change(`CREATE ${newIdx.unique ? "UNIQUE " : ""}INDEX ${ident(idx)} ON ${ident(name)}(${newIdx.cols.map(ident).join(", ")})`, `Created ${idx}.`)) setNewIdx({ cols: [], unique: false });
					}}>
						<select multiple size={Math.min(4, cols.length)} value={newIdx.cols} onChange={(e) => setNewIdx({ ...newIdx, cols: [...e.target.selectedOptions].map((o) => o.value) })}>
							{cols.map((c) => <option key={c.name}>{c.name}</option>)}
						</select>
						<label className="check"><input type="checkbox" checked={newIdx.unique} onChange={(e) => setNewIdx({ ...newIdx, unique: e.target.checked })} />unique</label>
						<button type="submit" disabled={!newIdx.cols.length}>＋ Index</button>
					</form>
				)}
			</section>
			<section>
				<h4>Definition</h4>
				<pre className="ddl">{obj.sql};</pre>
				{(table || obj.type === "view") && (
					<div className="inline">
						{table && <button onClick={() => {
							const to = prompt(`Rename table ${name} to:`, name);
							if (to && to !== name) change(`ALTER TABLE ${ident(name)} RENAME TO ${ident(to)}`, `Renamed ${name} to ${to}.`);
						}}>Rename…</button>}
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
