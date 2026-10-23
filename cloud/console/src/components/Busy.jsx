import { useEffect, useState } from "react";
import { onBusy } from "../db.js";

/** A thin bar across the top of the page while the database works longer than a blink. */
export function BusyBar() {
	const [on, setOn] = useState(false);
	useEffect(() => {
		let timer = null;
		const off = onBusy((n) => {
			clearTimeout(timer);
			if (n) timer = setTimeout(() => setOn(true), 150);
			else setOn(false);
		});
		return () => {
			off();
			clearTimeout(timer);
		};
	}, []);
	return <div className={`busybar${on ? " on" : ""}`} role="progressbar" aria-hidden={!on} />;
}

/** A centered spinner with a label, for a pane that has nothing to show yet. */
export function Spinner({ label = "Loading…" }) {
	return (
		<div className="spinner-wrap" role="status">
			<span className="spinner" />
			<span className="dim">{label}</span>
		</div>
	);
}
