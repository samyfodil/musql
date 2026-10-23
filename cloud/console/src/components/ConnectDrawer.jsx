import { useState } from "react";
import Drawer from "./Drawer.jsx";
import { Field } from "./ColumnPanel.jsx";
import { hostOf } from "../db.js";
import { validURL } from "../remotes.js";

/**
 * Asks for a remote database: a Hrana/libSQL URL, its token and a name to list
 * it under. onConnect({name, url, token}) tries it and resolves whether it
 * opened; the drawer stays open on a failure, with the fields as typed.
 */
export default function ConnectDrawer({ initial = {}, taken, onConnect, onClose }) {
	const [url, setUrl] = useState(initial.url ?? "");
	const [token, setToken] = useState(initial.token ?? "");
	const [name, setName] = useState(initial.name ?? "");
	const [trying, setTrying] = useState(false);
	const label = name.trim() || (validURL(url) ? hostOf(url.trim()) : "");
	const clash = label && taken?.(label);
	const ok = validURL(url) && label && !clash && !trying;

	async function go() {
		if (!ok) return;
		setTrying(true);
		try {
			await onConnect({ name: label, url: url.trim(), token: token.trim() });
		} finally {
			setTrying(false);
		}
	}

	return (
		<Drawer
			title="Connect to a remote database"
			subtitle="Any server that speaks Hrana over HTTP: musqld, sqld, Turso"
			onClose={onClose}
			footer={
				<>
					<span className="grow" />
					<button onClick={onClose}>Cancel</button>
					<button className="primary" disabled={!ok} onClick={go}>{trying ? <><span className="spinner inline" /> Connecting</> : "Connect"}</button>
				</>
			}
		>
			<form className="form" onSubmit={(e) => { e.preventDefault(); go(); }}>
				<Field label="URL" help="http://, https:// or libsql:// (libsql:// is https)">
					<input spellCheck={false} placeholder="libsql://my-db.example.com" value={url} onChange={(e) => setUrl(e.target.value)} />
				</Field>
				<Field label="Auth token" help="Kept in this browser's storage with the URL. Empty for a server that needs none.">
					<input type="password" spellCheck={false} autoComplete="off" value={token} onChange={(e) => setToken(e.target.value)} />
				</Field>
				<Field label="Name" help={clash ? "A database or connection in this browser already has that name." : "How the switcher lists it"}>
					<input spellCheck={false} placeholder={validURL(url) ? hostOf(url.trim()) : "my-db"} value={name} onChange={(e) => setName(e.target.value)} />
				</Field>
				<button type="submit" hidden />
			</form>
		</Drawer>
	);
}
