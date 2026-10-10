import { useEffect, useRef, useState } from "react";
import { show } from "../values.js";

/**
 * A result grid. Every prop past columns and rows is optional:
 *   types       column -> declared type, shown in the header
 *   sort/desc   the sorted column, and onSort(column) to change it
 *   picked      the selected row indexes, and onPick(i) to toggle one
 *   onEdit      (i, column, value) => Promise; makes cells editable by double-click
 *   start       the number of the first row
 */
export default function Grid({ columns, rows, types, sort, desc, onSort, picked, onPick, onEdit, start = 0 }) {
	const [editing, setEditing] = useState(null); // [row, col]
	return (
		<table className="grid">
			<thead>
				<tr>
					<th className="rn">#</th>
					{columns.map((c) => (
						<th key={c} className={sort === c ? "sorted" : ""} onClick={onSort && (() => onSort(c))}>
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
								}} />
							) : (
								<Cell key={j} v={v} onDoubleClick={onEdit && (() => setEditing([i, j]))} />
							),
						)}
					</tr>
				))}
			</tbody>
		</table>
	);
}

function Cell({ v, onDoubleClick }) {
	const { text, cls } = show(v);
	return (
		<td className={cls} title={typeof v === "string" && v.length > 40 ? v.slice(0, 2000) : undefined} onDoubleClick={onDoubleClick}>
			{text}
		</td>
	);
}

// EditCell: Enter saves, Escape cancels, Ctrl/Cmd+Backspace sets NULL, and
// leaving the cell saves.
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
