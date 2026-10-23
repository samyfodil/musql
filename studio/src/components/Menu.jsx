import { useEffect, useLayoutEffect, useRef, useState } from "react";

/**
 * A list of actions floating at x, y (a right-click) or under a button.
 * items: [{label, hint?, danger?, disabled?, onClick} | "-"]. Escape, a click
 * elsewhere or a pick closes it.
 */
export function MenuList({ items, x, y, onClose }) {
	const ref = useRef(null);
	const [pos, setPos] = useState({ left: x, top: y });
	useLayoutEffect(() => {
		const r = ref.current.getBoundingClientRect();
		setPos({ left: Math.max(4, Math.min(x, innerWidth - r.width - 8)), top: Math.max(4, Math.min(y, innerHeight - r.height - 8)) });
	}, [x, y]);
	useEffect(() => {
		const away = (e) => !ref.current?.contains(e.target) && onClose();
		const key = (e) => e.key === "Escape" && onClose();
		setTimeout(() => addEventListener("mousedown", away));
		addEventListener("keydown", key);
		addEventListener("blur", onClose);
		return () => {
			removeEventListener("mousedown", away);
			removeEventListener("keydown", key);
			removeEventListener("blur", onClose);
		};
	}, [onClose]);
	return (
		<div ref={ref} className="ctxmenu" style={pos} role="menu" onContextMenu={(e) => e.preventDefault()}>
			{items.filter(Boolean).map((it, i) =>
				it === "-" ? <div key={i} className="sep" /> : (
					<button key={i} role="menuitem" className={it.danger ? "danger-item" : ""} disabled={it.disabled} onClick={() => { onClose(); it.onClick(); }}>
						<span>{it.label}</span>
						{it.hint && <span className="dim small">{it.hint}</span>}
					</button>
				),
			)}
		</div>
	);
}

/** A button that opens a MenuList under it. */
export function MenuButton({ label, title, items, className = "" }) {
	const [at, setAt] = useState(null);
	return (
		<>
			<button className={className} title={title} aria-haspopup="menu" onClick={(e) => {
				const r = e.currentTarget.getBoundingClientRect();
				setAt(at ? null : { x: r.left, y: r.bottom + 4 });
			}}>{label}</button>
			{at && <MenuList items={items} x={at.x} y={at.y} onClose={() => setAt(null)} />}
		</>
	);
}

/** useContextMenu: [menu element, open(event, items)]. */
export function useContextMenu() {
	const [m, setM] = useState(null);
	const open = (e, items) => {
		e.preventDefault();
		setM({ x: e.clientX, y: e.clientY, items });
	};
	return [m && <MenuList {...m} onClose={() => setM(null)} />, open];
}
