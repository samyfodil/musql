import { useCallback, useState } from "react";

let seq = 0;

/** useToasts: [element, push(text, err)]. A message stays 3.5s, an error 7s, and a click dismisses it. */
export function useToasts() {
	const [list, setList] = useState([]);
	const drop = (id) => setList((l) => l.filter((t) => t.id !== id));
	const push = useCallback((text, err) => {
		const id = ++seq;
		setList((l) => [...l.filter((t) => t.text !== text).slice(-3), { id, text, err }]);
		setTimeout(() => drop(id), err ? 7000 : 3500);
	}, []);
	const el = (
		<div className="toasts" aria-live="polite">
			{list.map((t) => (
				<div key={t.id} className={`toast${t.err ? " err" : ""}`} onClick={() => drop(t.id)}>
					<span className="ti">{t.err ? "!" : "✓"}</span>
					<span>{t.text}</span>
				</div>
			))}
		</div>
	);
	return [el, push];
}
