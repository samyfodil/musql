import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { isVectorColumn, show } from "../values.js";
import { Copy, Maximize2, Pin } from "lucide-react";

const ROW = 32;
const OVERSCAN = 12;

/**
 * A virtual result grid: only the rows in view are in the page, so a table of
 * any size scrolls. Rows come from rowAt(i); one not loaded yet shows as a
 * placeholder and need(start, end) asks for it.
 *
 *   columns, count, rowAt(i), need?(start, end)
 *   types         column -> declared type (header label, how values show)
 *   layoutKey     where column widths, order and pins are remembered
 *   sort/desc, onSort(column), onHeaderMenu(event, column, gridItems)
 *   picked, onPick(i)       row selection (the # column)
 *   onOpen(i)               opens row i whole
 *   onEdit(i, column, text) makes cells editable
 *   onPaste(i, column, matrix), onClear(i0, i1, columns)   block writes
 *   onContextMenu(event, i, column, startEdit)
 *   range(i0, i1)           the rows i0..i1 for a copy (async), when not all loaded
 *
 * A click or the arrow keys move the current cell; Shift or a drag selects a
 * block, which Ctrl/Cmd+C copies as tab-separated text and Ctrl/Cmd+V fills
 * from a copied block. Enter or F2 edits, Delete empties the block, Space
 * opens the row.
 */
export default function Grid({ columns, count, rowAt, need, types, layoutKey, sort, desc, onSort, onHeaderMenu, picked, onPick, onOpen, onEdit, onPaste, onClear, onContextMenu, range }) {
	const scroller = useRef(null);
	const RN = Math.max(52, String(count).length * 8 + 34); // the # column fits the largest row number
	const [view, setView] = useState({ top: 0, height: 600 });
	const [sel, setSel] = useState(null); // {r0, c0, r1, c1}: anchor and focus, in display order
	const [editing, setEditing] = useState(null); // [row, displayCol]
	const [preview, setPreview] = useState(null); // {i, col, x, y}
	const dragging = useRef(false);
	const [layout, setLayout] = useLayout(layoutKey, columns, types, rowAt, count);

	const order = layout.order.filter((c) => columns.includes(c));
	const shownCols = [...layout.pinned.filter((c) => order.includes(c)), ...order.filter((c) => !layout.pinned.includes(c))];
	const idx = useMemo(() => Object.fromEntries(columns.map((c, j) => [c, j])), [columns]);
	const vector = useMemo(() => Object.fromEntries(columns.map((c) => [c, isVectorColumn(c, types?.[c])])), [columns, types]);
	const width = (c) => layout.widths[c] ?? 140;
	const left = {};
	let acc = RN;
	for (const c of shownCols) {
		if (layout.pinned.includes(c)) left[c] = acc;
		acc += width(c);
	}
	const total = acc;

	const first = Math.max(0, Math.floor(view.top / ROW) - OVERSCAN);
	const last = Math.min(count, Math.ceil((view.top + view.height) / ROW) + OVERSCAN);
	useEffect(() => {
		if (count) need?.(first, last);
	}, [first, last, count, need]);
	useLayoutEffect(() => {
		const el = scroller.current;
		const ro = new ResizeObserver(() => setView({ top: el.scrollTop, height: el.clientHeight }));
		ro.observe(el);
		return () => ro.disconnect();
	}, []);
	useEffect(() => setSel((s) => (s && s.r1 >= count ? null : s)), [count]);

	const cur = sel ? [sel.r1, sel.c1] : null;
	const rect = sel && { r0: Math.min(sel.r0, sel.r1), r1: Math.max(sel.r0, sel.r1), c0: Math.min(sel.c0, sel.c1), c1: Math.max(sel.c0, sel.c1) };
	const inSel = (i, j) => rect && i >= rect.r0 && i <= rect.r1 && j >= rect.c0 && j <= rect.c1;
	const multi = rect && (rect.r0 !== rect.r1 || rect.c0 !== rect.c1);

	const scrollTo = useCallback((i, j) => {
		const el = scroller.current;
		if (!el) return;
		const y = i * ROW;
		if (y < el.scrollTop) el.scrollTop = y;
		else if (y + ROW > el.scrollTop + el.clientHeight - ROW) el.scrollTop = y + 2 * ROW - el.clientHeight;
		const c = shownCols[j];
		if (c && left[c] === undefined) {
			let x = RN;
			for (const k of shownCols.slice(0, j)) x += width(k);
			const pinnedW = layout.pinned.reduce((n, p) => n + width(p), 0);
			if (x - pinnedW < el.scrollLeft) el.scrollLeft = x - RN - pinnedW;
			else if (x + width(c) > el.scrollLeft + el.clientWidth) el.scrollLeft = x + width(c) - el.clientWidth;
		}
	}, [shownCols, layout]);

	const move = (r, c, extend) => {
		r = Math.max(0, Math.min(count - 1, r));
		c = Math.max(0, Math.min(shownCols.length - 1, c));
		setSel((s) => (extend && s ? { ...s, r1: r, c1: c } : { r0: r, c0: c, r1: r, c1: c }));
		scrollTo(r, c);
	};

	async function rowsOf(r0, r1) {
		const out = [];
		let missing = false;
		for (let i = r0; i <= r1; i++) {
			const row = rowAt(i);
			if (!row) missing = true;
			out.push(row);
		}
		return missing && range ? range(r0, r1) : out;
	}

	async function copy() {
		if (!rect) return;
		const rs = await rowsOf(rect.r0, rect.r1);
		const cs = shownCols.slice(rect.c0, rect.c1 + 1);
		const text = rs.map((row) => cs.map((c) => tsvCell(row?.[idx[c]])).join("\t")).join("\n");
		await navigator.clipboard?.writeText(text);
	}

	function onKeyDown(e) {
		if (editing || !count) return;
		const mod = e.metaKey || e.ctrlKey;
		if (mod && e.key.toLowerCase() === "a") {
			e.preventDefault();
			return setSel({ r0: 0, c0: 0, r1: count - 1, c1: shownCols.length - 1 });
		}
		if (!cur) {
			if (/^Arrow|^Home$|^End$/.test(e.key)) {
				e.preventDefault();
				move(0, 0);
			}
			return;
		}
		const [i, j] = cur;
		const page = Math.max(1, Math.floor(view.height / ROW) - 1);
		const go = (r, c) => {
			e.preventDefault();
			move(r, c, e.shiftKey && !/^Tab$/.test(e.key));
		};
		switch (e.key) {
			case "ArrowUp": return go(mod ? 0 : i - 1, j);
			case "ArrowDown": return go(mod ? count - 1 : i + 1, j);
			case "ArrowLeft": return go(i, mod ? 0 : j - 1);
			case "ArrowRight": return go(i, mod ? shownCols.length - 1 : j + 1);
			case "Tab": return go(i, e.shiftKey ? j - 1 : j + 1);
			case "Home": return go(mod ? 0 : i, 0);
			case "End": return go(mod ? count - 1 : i, shownCols.length - 1);
			case "PageUp": return go(i - page, j);
			case "PageDown": return go(i + page, j);
			case "Escape": return setSel(null);
			case "Enter":
			case "F2":
				if (onEdit && !multi) {
					e.preventDefault();
					setEditing([i, j]);
				}
				return;
			case " ":
				if (onOpen) {
					e.preventDefault();
					onOpen(i);
				}
				return;
			case "Delete":
			case "Backspace":
				if (onClear && rect) {
					e.preventDefault();
					onClear(rect.r0, rect.r1, shownCols.slice(rect.c0, rect.c1 + 1));
				}
				return;
		}
		if (mod && e.key.toLowerCase() === "c" && !getSelection().toString()) {
			e.preventDefault();
			copy();
		}
	}

	function onPasteEvent(e) {
		if (!onPaste || !cur || editing) return;
		const text = e.clipboardData.getData("text/plain");
		if (!text) return;
		e.preventDefault();
		const matrix = parseTSV(text);
		const cs = shownCols.slice(cur[1], cur[1] + matrix[0].length);
		onPaste(cur[0], cs, matrix.map((r) => r.slice(0, cs.length)));
	}

	// Column resize: drag a header's right edge; a double-click fits it.
	function startResize(e, c) {
		e.preventDefault();
		e.stopPropagation();
		const x0 = e.clientX;
		const w0 = width(c);
		const onMove = (ev) => setLayout((l) => ({ ...l, widths: { ...l.widths, [c]: Math.max(48, w0 + ev.clientX - x0) } }));
		const onUp = () => {
			removeEventListener("mousemove", onMove);
			removeEventListener("mouseup", onUp);
		};
		addEventListener("mousemove", onMove);
		addEventListener("mouseup", onUp);
	}
	const fit = (c) => setLayout((l) => ({ ...l, widths: { ...l.widths, [c]: fitWidth(c, types?.[c], (i) => rowAt(i)?.[idx[c]], count, vector[c], first, last) } }));
	const togglePin = (c) => setLayout((l) => ({ ...l, pinned: l.pinned.includes(c) ? l.pinned.filter((p) => p !== c) : [...l.pinned, c] }));

	const dragCol = useRef(null);
	const gridItems = (c) => [
		{ label: layout.pinned.includes(c) ? "Unpin column" : "Pin column", onClick: () => togglePin(c) },
		{ label: "Fit width to content", onClick: () => fit(c) },
		layout.pinned.length || Object.keys(layout.widths).length ? { label: "Reset column layout", onClick: () => setLayout(defaultLayout(columns, types, rowAt, count)) } : null,
	];

	return (
		<div
			ref={scroller}
			className="vscroll"
			tabIndex={0}
			onScroll={(e) => setView({ top: e.currentTarget.scrollTop, height: e.currentTarget.clientHeight })}
			onKeyDown={onKeyDown}
			onPaste={onPasteEvent}
			onCopy={(e) => {
				if (rect && !editing && !getSelection().toString()) {
					e.preventDefault();
					copy();
				}
			}}
		>
			<table className="grid vgrid" style={{ width: total }}>
				<colgroup>
					<col style={{ width: RN }} />
					{shownCols.map((c) => <col key={c} style={{ width: width(c) }} />)}
				</colgroup>
				<thead>
					<tr>
						<th className="rn" onClick={() => count && setSel({ r0: 0, c0: 0, r1: count - 1, c1: shownCols.length - 1 })} title="Select all (⌘A)">#</th>
						{shownCols.map((c, j) => (
							<th
								key={c}
								className={`${sort === c ? "sorted" : ""}${layout.pinned.includes(c) ? " pinned" : ""}${rect && j >= rect.c0 && j <= rect.c1 ? " colsel" : ""}`}
								style={left[c] !== undefined ? { left: left[c] } : undefined}
								draggable
								onDragStart={(e) => { dragCol.current = c; e.dataTransfer.effectAllowed = "move"; }}
								onDragOver={(e) => { if (dragCol.current && dragCol.current !== c) e.preventDefault(); }}
								onDrop={(e) => {
									e.preventDefault();
									const from = dragCol.current;
									dragCol.current = null;
									if (!from || from === c) return;
									setLayout((l) => {
										const o = l.order.filter((x) => x !== from);
										o.splice(o.indexOf(c), 0, from);
										return { ...l, order: o };
									});
								}}
								onClick={onSort && (() => onSort(c))}
								onContextMenu={onHeaderMenu && ((e) => onHeaderMenu(e, c, gridItems(c)))}
							>
								<div className="thin">
									{layout.pinned.includes(c) && <Pin size={11} className="pinmark" />}
									<span className="thname">{c}</span>
									{types?.[c] && <span className="type">{types[c]}</span>}
									{sort === c && <span className="sortmark">{desc ? "↓" : "↑"}</span>}
									{onHeaderMenu && <button className="thmenu" title="Sort, filter, pin or change this column" onClick={(e) => { e.stopPropagation(); onHeaderMenu(e, c, gridItems(c)); }}>▾</button>}
								</div>
								<span className="resizer" onMouseDown={(e) => startResize(e, c)} onDoubleClick={(e) => { e.stopPropagation(); fit(c); }} onClick={(e) => e.stopPropagation()} />
							</th>
						))}
					</tr>
				</thead>
				<tbody>
					{first > 0 && <tr className="spacer" style={{ height: first * ROW }}><td colSpan={shownCols.length + 1} /></tr>}
					{Array.from({ length: last - first }, (_, k) => {
						const i = first + k;
						const row = rowAt(i);
						if (!row) {
							return (
								<tr key={i} className="skel">
									<td className="rn">{i + 1}</td>
									{shownCols.map((c) => <td key={c} style={left[c] !== undefined ? { left: left[c] } : undefined} className={left[c] !== undefined ? "pinned" : ""}><span className="bone" style={{ width: `${30 + ((i * 7 + idx[c] * 13) % 55)}%` }} /></td>)}
								</tr>
							);
						}
						return (
							<tr key={i} className={picked?.has(i) ? "picked" : ""}>
								<td className={`rn${onPick ? " pickable" : ""}`} onClick={onPick && (() => onPick(i))}>
									{onPick && <input type="checkbox" tabIndex={-1} checked={!!picked?.has(i)} readOnly />}
									<span>{i + 1}</span>
									{onOpen && <button className="expand" title="Open the row (Space)" onClick={(e) => { e.stopPropagation(); onOpen(i); }}><Maximize2 size={12} /></button>}
								</td>
								{shownCols.map((c, j) => {
									const v = row[idx[c]];
									const pinStyle = left[c] !== undefined ? { left: left[c] } : undefined;
									if (editing?.[0] === i && editing?.[1] === j) {
										return (
											<EditCell key={c} value={v} style={pinStyle} onDone={async (save, value) => {
												setEditing(null);
												scroller.current?.focus();
												if (save) await onEdit(i, c, value);
											}} />
										);
									}
									const s = show(v, vector[c], types?.[c], c);
									const isCur = cur?.[0] === i && cur?.[1] === j;
									return (
										<td
											key={c}
											style={pinStyle}
											className={`${s.cls ?? ""}${left[c] !== undefined ? " pinned" : ""}${inSel(i, j) ? (multi ? " insel" : "") : ""}${isCur ? " cur" : ""}`}
											title={s.title}
											onMouseDown={(e) => {
												if (e.button !== 0) return;
												dragging.current = true;
												const up = () => { dragging.current = false; removeEventListener("mouseup", up); };
												addEventListener("mouseup", up);
												setSel((p) => (e.shiftKey && p ? { ...p, r1: i, c1: j } : { r0: i, c0: j, r1: i, c1: j }));
											}}
											onMouseEnter={() => dragging.current && setSel((p) => (p ? { ...p, r1: i, c1: j } : p))}
											onDoubleClick={onEdit && (() => (s.long ? setPreview({ i, col: c }) : setEditing([i, j])))}
											onContextMenu={onContextMenu && ((e) => {
												if (!inSel(i, j)) setSel({ r0: i, c0: j, r1: i, c1: j });
												onContextMenu(e, i, c, () => (s.long ? setPreview({ i, col: c }) : setEditing([i, j])));
											})}
										>
											<span className="cv">{s.text}</span>
											{s.long && <button className="peek" title="Show it all" onMouseDown={(e) => e.stopPropagation()} onClick={(e) => { e.stopPropagation(); setPreview({ i, col: c }); }}><Maximize2 size={11} /></button>}
										</td>
									);
								})}
							</tr>
						);
					})}
					{last < count && <tr className="spacer" style={{ height: (count - last) * ROW }}><td colSpan={shownCols.length + 1} /></tr>}
				</tbody>
			</table>
			{multi && <SelectionStat rect={rect} rowAt={rowAt} cols={shownCols} idx={idx} />}
			{preview && rowAt(preview.i) && (
				<ValuePreview
					value={rowAt(preview.i)[idx[preview.col]]}
					column={preview.col}
					onSave={onEdit && (async (text) => { setPreview(null); await onEdit(preview.i, preview.col, text); })}
					onClose={() => { setPreview(null); scroller.current?.focus(); }}
				/>
			)}
		</div>
	);
}

// SelectionStat is the corner summary of a selected block: how many cells,
// and the sum and average of the numbers in it, as a spreadsheet shows.
function SelectionStat({ rect, rowAt, cols, idx }) {
	let n = 0;
	let sum = 0;
	let nums = 0;
	for (let i = rect.r0; i <= Math.min(rect.r1, rect.r0 + 5000); i++) {
		const row = rowAt(i);
		for (let j = rect.c0; j <= rect.c1; j++) {
			n++;
			const v = row?.[idx[cols[j]]];
			if (typeof v === "number" || typeof v === "bigint") {
				sum += Number(v);
				nums++;
			}
		}
	}
	const cells = (rect.r1 - rect.r0 + 1) * (rect.c1 - rect.c0 + 1);
	return createPortal(
		<div className="selstat">
			{cells.toLocaleString()} cells
			{nums > 0 && <> · sum {round(sum)} · avg {round(sum / nums)}</>}
			{n < cells && " (first 5,000 rows)"}
			<span className="dim"> · ⌘C copies</span>
		</div>,
		document.body,
	);
}
const round = (x) => (Number.isInteger(x) ? x.toLocaleString() : x.toLocaleString(undefined, { maximumFractionDigits: 3 }));

// ValuePreview shows a long text or JSON value whole, pretty-printed, to copy
// or (with onSave) edit in a roomy box.
function ValuePreview({ value, column, onSave, onClose }) {
	const text = value === null ? "" : value instanceof Uint8Array ? null : String(value);
	let pretty = text;
	let json = false;
	try {
		if (text && /^\s*[[{]/.test(text)) {
			pretty = JSON.stringify(JSON.parse(text), null, 2);
			json = true;
		}
	} catch {
		json = false;
	}
	const [draft, setDraft] = useState(pretty ?? "");
	const ref = useRef(null);
	useEffect(() => {
		ref.current?.focus();
		const key = (e) => e.key === "Escape" && onClose();
		addEventListener("keydown", key);
		return () => removeEventListener("keydown", key);
	}, []);
	const save = () => onSave(json ? JSON.stringify(JSON.parse(draft)) : draft);
	let bad = null;
	if (json) {
		try {
			JSON.parse(draft);
		} catch (e) {
			bad = e.message;
		}
	}
	return createPortal(
		<div className="preview-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
			<div className="preview">
				<header>
					<strong className="mono">{column}</strong>
					<span className="dim small">{json ? "JSON" : text === null ? "BLOB" : `${(text ?? "").length.toLocaleString()} characters`}</span>
					<span className="grow" />
					<button className="ghost icon" title="Copy" onClick={() => navigator.clipboard?.writeText(onSave ? draft : pretty ?? "")}><Copy size={14} /></button>
					<button className="ghost icon" title="Close (Esc)" onClick={onClose}>✕</button>
				</header>
				{text === null ? (
					<p className="dim pad">A BLOB: change it with SQL.</p>
				) : (
					<textarea
						ref={ref}
						spellCheck={false}
						readOnly={!onSave}
						value={onSave ? draft : pretty}
						onChange={(e) => setDraft(e.target.value)}
						onKeyDown={(e) => {
							if (e.key === "Enter" && (e.metaKey || e.ctrlKey) && onSave && !bad) save();
						}}
					/>
				)}
				{onSave && text !== null && (
					<footer>
						{bad ? <span className="err small">Not valid JSON: {bad}</span> : <span className="dim small">⌘Enter saves</span>}
						<span className="grow" />
						<button onClick={onClose}>Cancel</button>
						<button className="primary" disabled={!!bad || draft === pretty} onClick={save}>Save</button>
					</footer>
				)}
			</div>
		</div>,
		document.body,
	);
}

// EditCell: Enter saves, Escape cancels, Ctrl/Cmd+Backspace sets NULL, and
// leaving the cell saves. The text is stored by the column's type
// (values.js typed): '…' in single quotes is always text.
function EditCell({ value, onDone, style }) {
	const ref = useRef(null);
	const done = useRef(false);
	const orig = value === null ? null : String(value);
	useEffect(() => {
		ref.current.focus();
		ref.current.select();
	}, []);
	const finish = (save, v) => {
		if (done.current) return;
		done.current = true;
		onDone(save && v !== orig, v);
	};
	return (
		<td className="editing" style={style}>
			<input
				ref={ref}
				defaultValue={orig ?? ""}
				spellCheck={false}
				placeholder="NULL"
				title="Enter saves · Esc cancels · ⌘/Ctrl+Backspace sets NULL · '…' keeps a number as text"
				onKeyDown={(e) => {
					e.stopPropagation();
					if (e.key === "Enter") finish(true, e.currentTarget.value);
					else if (e.key === "Escape") finish(false);
					else if (e.key === "Backspace" && (e.metaKey || e.ctrlKey)) {
						e.preventDefault();
						finish(true, null);
					}
				}}
				onBlur={(e) => finish(true, e.currentTarget.value)}
			/>
		</td>
	);
}

/** A value as one cell of tab-separated text: NULL empty, a tab or newline quoted. */
function tsvCell(v) {
	if (v === null || v === undefined) return "";
	if (v instanceof Uint8Array) return `x'${Array.from(v, (b) => b.toString(16).padStart(2, "0")).join("")}'`;
	const s = String(v);
	return /[\t\n"]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

/** Tab-separated text (as spreadsheets copy it) as rows of cells; quoted cells may hold tabs and newlines. */
export function parseTSV(text) {
	const rows = [[]];
	let cell = "";
	let quoted = false;
	const s = text.replace(/\r\n?/g, "\n").replace(/\n$/, "");
	for (let i = 0; i < s.length; i++) {
		const ch = s[i];
		if (quoted) {
			if (ch === '"' && s[i + 1] === '"') { cell += '"'; i++; }
			else if (ch === '"') quoted = false;
			else cell += ch;
		} else if (ch === '"' && cell === "") quoted = true;
		else if (ch === "\t") { rows.at(-1).push(cell); cell = ""; }
		else if (ch === "\n") { rows.at(-1).push(cell); rows.push([]); cell = ""; }
		else cell += ch;
	}
	rows.at(-1).push(cell);
	return rows;
}

// ---- column layout ----

// fitWidth is a width that shows column c's header and its values in view,
// within reason: get(i) is row i's value, undefined when not loaded.
function fitWidth(c, type, get, count, vector, first = 0, last = 60) {
	let chars = c.length + (type ? type.length * 0.8 + 2 : 0);
	for (let i = first; i < Math.min(count, Math.max(last, first + 60)); i++) {
		const v = get(i);
		if (v === undefined) continue;
		chars = Math.max(chars, Math.min(String(show(v, vector, type, c).text).length, 60));
	}
	return Math.round(Math.min(420, Math.max(64, chars * 7.4 + 34)));
}

function defaultLayout(columns, types) {
	const widths = {};
	for (const c of columns) {
		const t = (types?.[c] ?? "").toUpperCase();
		const key = c === "id" || /_id$/.test(c) || c === "rowid";
		widths[c] = key ? 84 : /INT|REAL|NUM|DEC|FLOA|DOUB|BOOL/.test(t) ? 110 : /DATE|TIME/.test(t) ? 170 : /BLOB/.test(t) ? 150 : 220;
	}
	return { widths, order: [...columns], pinned: [] };
}

// useLayout keeps column widths, order and pins, per layoutKey, in this
// browser. New columns join the order at its end; widths start from the type.
function useLayout(key, columns, types) {
	const storageKey = key ? `musql-studio:cols:${key}` : null;
	const [layout, setLayoutState] = useState(() => load(storageKey, columns, types));
	const sig = columns.join("\0");
	useEffect(() => setLayoutState(load(storageKey, columns, types)), [storageKey, sig]);
	const setLayout = useCallback((f) => setLayoutState((l) => {
		const next = typeof f === "function" ? f(l) : f;
		try {
			if (storageKey) localStorage.setItem(storageKey, JSON.stringify(next));
		} catch {
			// layout is kept for this page only
		}
		return next;
	}), [storageKey]);
	return [layout, setLayout];
}

function load(storageKey, columns, types) {
	const base = defaultLayout(columns, types);
	let saved = null;
	try {
		saved = storageKey ? JSON.parse(localStorage.getItem(storageKey) ?? "null") : null;
	} catch {
		saved = null;
	}
	if (!saved) return base;
	const order = [...(saved.order ?? []).filter((c) => columns.includes(c)), ...columns.filter((c) => !(saved.order ?? []).includes(c))];
	return { widths: { ...base.widths, ...saved.widths }, order, pinned: (saved.pinned ?? []).filter((c) => columns.includes(c)) };
}
