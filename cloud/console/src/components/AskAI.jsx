import { useState } from "react";
import { api, message } from "../cloud/api.js";
import { Spinner } from "./Busy.jsx";

// AskAI turns a request into SQL for a cloud database and hands it to the
// editor. It never runs anything: the user reads it and presses Run.
export default function AskAI({ target, onInsert }) {
	const [prompt, setPrompt] = useState("");
	const [busy, setBusy] = useState(false);
	const [out, setOut] = useState(null); // {writes, explanation} | {error}

	async function go() {
		if (!prompt.trim() || busy) return;
		setBusy(true);
		setOut(null);
		try {
			const r = await api.ai.generateSQL({ org: target.org, database: target.database, prompt });
			onInsert(r.sql);
			setOut({ writes: r.writes, explanation: r.explanation });
		} catch (e) {
			setOut({ error: message(e) });
		} finally {
			setBusy(false);
		}
	}

	return (
		<div className="askai toolbar">
			<input
				className="where"
				placeholder="Describe the query you want…"
				value={prompt}
				onChange={(e) => setPrompt(e.target.value)}
				onKeyDown={(e) => e.key === "Enter" && go()}
			/>
			<button className="primary" disabled={busy || !prompt.trim()} onClick={go}>{busy ? <Spinner label="Asking" /> : "Generate"}</button>
			{out?.error && <span className="dim small" role="alert">{out.error}</span>}
			{out && !out.error && (
				<span className="small" title={out.explanation}>
					{out.writes ? "Writes data: review it before you run it." : "Read-only."} {out.explanation}
				</span>
			)}
		</div>
	);
}
