import { useEffect, useRef, useState } from "react";

/** A button that opens a floating panel under it; a click outside or Escape closes it. */
export default function Popover({ label, className = "", title, children, width = 420 }) {
	const [open, setOpen] = useState(false);
	const box = useRef(null);
	useEffect(() => {
		if (!open) return;
		const away = (e) => !box.current?.contains(e.target) && setOpen(false);
		const key = (e) => e.key === "Escape" && setOpen(false);
		addEventListener("mousedown", away);
		addEventListener("keydown", key);
		return () => {
			removeEventListener("mousedown", away);
			removeEventListener("keydown", key);
		};
	}, [open]);
	return (
		<div className="popover-anchor" ref={box}>
			<button className={`${className}${open ? " pressed" : ""}`} title={title} aria-expanded={open} onClick={() => setOpen(!open)}>{label}</button>
			{open && <div className="popover" style={{ width }}>{typeof children === "function" ? children(() => setOpen(false)) : children}</div>}
		</div>
	);
}
