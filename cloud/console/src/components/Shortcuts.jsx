import { useEffect } from "react";

const GROUPS = [
	["Anywhere", [["⌘K", "Search tables, queries and actions"], ["⌘Z / ⇧⌘Z", "Undo / redo the last change"], ["⌘O", "Open a file"], ["?", "These shortcuts"]]],
	["Grid", [["↑ ↓ ← →", "Move; with ⇧, select a block"], ["⌘ + arrow", "Jump to the edge"], ["Enter / F2", "Edit the cell"], ["Space", "Open the row"], ["⌘C / ⌘V", "Copy or paste a block, as a spreadsheet does"], ["Delete", "Empty the selected cells"], ["⌘A", "Select everything"], ["Double-click a header edge", "Fit the column"]]],
	["SQL editor", [["⌘Enter", "Run the statement at the cursor, or the selection"], ["⇧⌘Enter", "Run every statement"], ["⇧⌥F", "Format the SQL"]]],
	["Editing a cell", [["Enter", "Save"], ["Esc", "Cancel"], ["⌘Backspace", "Set NULL"], ["'42'", "Quotes keep a number as text"]]],
];

/** The keyboard shortcuts, by where they work. */
export default function Shortcuts({ onClose }) {
	useEffect(() => {
		const key = (e) => (e.key === "Escape" || e.key === "?") && onClose();
		addEventListener("keydown", key);
		return () => removeEventListener("keydown", key);
	}, [onClose]);
	return (
		<div className="palette-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
			<div className="shortcuts" role="dialog" aria-label="Keyboard shortcuts">
				<header><strong>Keyboard shortcuts</strong><button className="ghost icon" onClick={onClose}>✕</button></header>
				<div className="sc-groups">
					{GROUPS.map(([g, list]) => (
						<section key={g}>
							<h4>{g}</h4>
							{list.map(([k, what]) => (
								<div key={k} className="sc-row"><span>{what}</span><kbd>{k}</kbd></div>
							))}
						</section>
					))}
				</div>
			</div>
		</div>
	);
}
