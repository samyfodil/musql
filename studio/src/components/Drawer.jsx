import { useEffect, useRef, useState } from "react";

/**
 * A panel that slides in from the right over the page, as Supabase edits a row
 * or a column: a title, a scrolling body and a footer of actions. Escape or a
 * click on the backdrop closes it.
 */
export default function Drawer({ title, subtitle, children, footer, onClose, wide }) {
	const [shown, setShown] = useState(false);
	const panel = useRef(null);
	useEffect(() => {
		requestAnimationFrame(() => setShown(true));
		const key = (e) => e.key === "Escape" && !e.defaultPrevented && onClose();
		addEventListener("keydown", key);
		const first = panel.current?.querySelector("input, select, textarea, button.primary");
		first?.focus();
		return () => removeEventListener("keydown", key);
	}, []);
	return (
		<div className={`drawer-backdrop${shown ? " in" : ""}`} onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
			<aside ref={panel} className={`drawer${wide ? " wide" : ""}${shown ? " in" : ""}`} role="dialog" aria-label={title}>
				<header>
					<div>
						<h2>{title}</h2>
						{subtitle && <p className="dim small">{subtitle}</p>}
					</div>
					<button className="ghost icon" title="Close (Esc)" onClick={onClose}>✕</button>
				</header>
				<div className="drawer-body">{children}</div>
				{footer && <footer>{footer}</footer>}
			</aside>
		</div>
	);
}
