import { useEffect, useMemo, useRef, useState } from "react";

/**
 * The command palette (⌘K): every table, view and saved query, and every
 * action, found by typing. commands: [{label, group, hint?, run}].
 */
export default function Palette({ commands, onClose }) {
	const [q, setQ] = useState("");
	const [at, setAt] = useState(0);
	const list = useRef(null);
	const found = useMemo(() => {
		const words = q.toLowerCase().split(/\s+/).filter(Boolean);
		return commands.filter((c) => words.every((w) => `${c.group} ${c.label}`.toLowerCase().includes(w))).slice(0, 60);
	}, [q, commands]);
	useEffect(() => setAt(0), [q]);
	useEffect(() => list.current?.querySelector(".on")?.scrollIntoView({ block: "nearest" }), [at]);
	const pick = (c) => {
		onClose();
		c.run();
	};
	let group = null;
	return (
		<div className="palette-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
			<div className="palette" role="dialog" aria-label="Command palette">
				<input
					autoFocus
					spellCheck={false}
					placeholder="Search tables, queries and actions…"
					value={q}
					onChange={(e) => setQ(e.target.value)}
					onKeyDown={(e) => {
						if (e.key === "ArrowDown") { e.preventDefault(); setAt(Math.min(at + 1, found.length - 1)); }
						else if (e.key === "ArrowUp") { e.preventDefault(); setAt(Math.max(at - 1, 0)); }
						else if (e.key === "Enter" && found[at]) pick(found[at]);
						else if (e.key === "Escape") onClose();
					}}
				/>
				<div className="palette-list" ref={list}>
					{!found.length && <p className="dim pad">Nothing matches “{q}”.</p>}
					{found.map((c, i) => {
						const head = c.group !== group ? (group = c.group) : null;
						return (
							<div key={`${c.group}:${c.label}`}>
								{head && <div className="palette-group">{head}</div>}
								<button className={i === at ? "on" : ""} onMouseMove={() => setAt(i)} onClick={() => pick(c)}>
									<span className="pi">{c.icon ?? "›"}</span>
									<span className="pl">{c.label}</span>
									{c.hint && <span className="dim small">{c.hint}</span>}
								</button>
							</div>
						);
					})}
				</div>
				<div className="palette-foot dim small">↑↓ to move · ⏎ to open · esc to close</div>
			</div>
		</div>
	);
}
