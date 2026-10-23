import { useEffect, useRef, useState } from "react";
import { isVectorColumn, show } from "../values.js";

/**
 * A result grid. Every prop past columns and rows is optional:
 *   types       column -> declared type, shown in the header; a vector type
 *               shows its column's values as vectors
 *   sort/desc   the sorted column, and onSort(column) to change it
 *   picked      the selected row indexes, and onPick(i) to toggle one
 *   onEdit      (i, column, text) => Promise; makes cells editable by double-click
 *   start       the number of the first row
 *
 * A click or the arrow keys move the current cell; Enter or F2 edits it,
 * Space selects its row, and Ctrl/Cmd+C copies its value.
 */
export default function Grid({ columns, rows, types, sort, desc, onSort, picked, onPick, onEdit, start = 0 }) {
	const [editing, setEditing] = useState(null); // [row, col]
	const [cur, setCur] = useState(null); // [row, col]
	const table = useRef(null);
	const vector = columns.map((c) => isVectorColumn(c, types?.[c]));
	useEffect(() => setCur(null), [rows]);
	useEffect(() => {
		if (cur) table.current?.querySelector(`tbody tr:nth-child(${cur[0] + 1}) td:nth-child(${cur[1] + 2})`)?.scrollIntoView({ block: "nearest", inline: "nearest" });
	}, [cur]);

	function onKeyDown(e) {
		if (editing || !rows.length) return;
		const [i, j] = cur ?? [0, 0];
		const to = (r, c) => {
			e.preventDefault();
			setCur([Math.max(0, Math.min(rows.length - 1, r)), Math.max(0, Math.min(columns.length - 1, c))]);
		};
		if (!cur && /^Arrow|^Home$|^End$/.test(e.key)) return to(0, 0);
		if (!cur) return;
		switch (e.key) {
			case "ArrowUp": return to(i - 1, j);
			case "ArrowDown": return to(i + 1, j);
			case "ArrowLeft": return to(i, j - 1);
			case "ArrowRight": return to(i, j + 1);
			case "Tab": return to(i, e.shiftKey ? j - 1 : j + 1);
			case "Home": return to(e.metaKey || e.ctrlKey ? 0 : i, 0);
			case "End": return to(e.metaKey || e.ctrlKey ? rows.length - 1 : i, columns.length - 1);
			case "PageUp": return to(i - 20, j);
			case "PageDown": return to(i + 20, j);
			case "Escape": return setCur(null);
			case "Enter":
			case "F2":
				if (onEdit) {
					e.preventDefault();
					setEditing([i, j]);
				}
				return;
			case " ":
				if (onPick) {
					e.preventDefault();
					onPick(i);
				}
				return;
		}
		if (e.key.toLowerCase() === "c" && (e.metaKey || e.ctrlKey) && !getSelection().toString()) {
			e.preventDefault();
			navigator.clipboard?.writeText(show(rows[i][j], vector[j]).text);
		}
	}

	return (
		<table className="grid" ref={table} tabIndex={0} onKeyDown={onKeyDown}>
			<thead>
				<tr>
					<th className="rn">#</th>
					{columns.map((c, j) => (
						<th key={j} className={sort === c ? "sorted" : ""} onClick={onSort && (() => onSort(c))}>
							{c}
							{types?.[c] && <span className="type">{types[c]}</span>}
							{sort === c && (desc ? " ↓" : " ↑")}
						</th>
					))}
				</tr>
			</thead>
			<tbody>
				{rows.map((row, i) => (
					<tr key={start + i} className={picked?.has(i) ? "picked" : ""}>
						<td className="rn" onClick={onPick && (() => onPick(i))}>{start + i + 1}</td>
						{row.map((v, j) =>
							editing?.[0] === i && editing?.[1] === j ? (
								<EditCell key={j} value={v} onDone={async (save, value) => {
									if (save) await onEdit(i, columns[j], value);
									setEditing(null);
									table.current?.focus();
								}} />
							) : (
								<Cell key={j} v={v} vector={vector[j]} cur={cur?.[0] === i && cur?.[1] === j} onClick={() => setCur([i, j])} onDoubleClick={onEdit && (() => setEditing([i, j]))} />
							),
						)}
					</tr>
				))}
			</tbody>
		</table>
	);
}

function Cell({ v, vector, cur, onClick, onDoubleClick }) {
	const { text, cls, title } = show(v, vector);
	return (
		<td className={cur ? `${cls ?? ""} cur` : cls} title={title} onClick={onClick} onDoubleClick={onDoubleClick}>
			{text}
		</td>
	);
}

// EditCell: Enter saves, Escape cancels, Ctrl/Cmd+Backspace sets NULL, and
// leaving the cell saves. The text is stored by the column's type
// (values.js typed): '…' in single quotes is always text.
function EditCell({ value, onDone }) {
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
		<td className="editing">
			<input
				ref={ref}
				defaultValue={orig ?? ""}
				spellCheck={false}
				placeholder="NULL"
				title="Enter saves · Esc cancels · ⌘/Ctrl+Backspace sets NULL · '…' keeps a number as text"
				onKeyDown={(e) => {
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
