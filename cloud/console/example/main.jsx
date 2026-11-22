// The stock console plus a page, a database tab, a banner, a theme and a
// name, all through createConsole.
import { useEffect, useState } from "react";
import { CreditCard, Gauge } from "lucide-react";
import { createConsole, useConsole } from "@samyfodil/musql-console";

const logo = `data:image/svg+xml,${encodeURIComponent('<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="8" fill="#0b7285"/><text x="16" y="22" font-family="sans-serif" font-size="16" fill="#fff" text-anchor="middle">ex</text></svg>')}`;

const bytes = (n) => `${(Number(n) / 1024).toFixed(1)} KB`;

// An org-level page: it calls the API with the signed-in user's token, as a
// page calling its own service through ctx.transport would.
function Billing({ ctx }) {
	const [dbs, setDbs] = useState(null);
	useEffect(() => {
		ctx.api.db.list({ org: ctx.org.slug }).then((r) => setDbs(r.databases), (e) => ctx.toast(e.message, true));
	}, [ctx.org.slug]);
	const total = dbs?.reduce((n, d) => n + Number(d.sizeBytes), 0) ?? 0;
	return (
		<>
			<header className="cloudhead"><div><h1>Billing</h1><p className="dim small">{ctx.org.name || ctx.org.slug} · {ctx.role}</p></div></header>
			<table className="cloudtable kv">
				<tbody>
					<tr><th>Plan</th><td>Free</td></tr>
					<tr><th>Databases</th><td>{dbs ? dbs.length : "…"}</td></tr>
					<tr><th>Storage</th><td>{bytes(total)} of 5 GB</td></tr>
					<tr><th>Signed in as</th><td>{ctx.user?.email}</td></tr>
				</tbody>
			</table>
			<p><button className="primary" onClick={() => ctx.toast("Upgrades are not part of the example.")}>Upgrade</button></p>
		</>
	);
}

// A tab on the database open in the editor.
function Quota() {
	const ctx = useConsole();
	const { org, database } = ctx.database;
	const [db, setDb] = useState(null);
	useEffect(() => {
		ctx.api.db.get({ org, name: database }).then((r) => setDb(r.database), (e) => ctx.toast(e.message, true));
	}, [org, database]);
	return (
		<>
			<header className="cloudhead"><div><h1>Quota</h1><p className="dim small mono">{org}/{database}</p></div></header>
			<table className="cloudtable kv">
				<tbody>
					<tr><th>Size</th><td>{db ? bytes(db.sizeBytes) : "…"} of 1 GB</td></tr>
					<tr><th>Rows read</th><td>0 of 500 million this month</td></tr>
				</tbody>
			</table>
		</>
	);
}

function PlanBanner({ ctx }) {
	if (!ctx.org) return null;
	return (
		<div className="banner">
			<b>Free plan</b>
			<span>{ctx.org.slug} has 5 GB of storage.</span>
			<span className="grow" />
			<button onClick={() => ctx.navigate("billing")}>Billing</button>
		</div>
	);
}

createConsole(document.getElementById("app"), {
	branding: { name: "Example cloud", studioName: "Example studio", logo, favicon: logo, docsURL: "https://github.com/samyfodil/musql/tree/main/cloud/console", supportURL: "https://github.com/samyfodil/musql/issues" },
	theme: {
		light: { accent: "#0b7285", "accent-soft": "#e3fafc", sel: "#e6fcf5" },
		dark: { accent: "#3bc9db", "accent-soft": "#0c2e33", sel: "#10292c" },
	},
	pages: [{ key: "billing", label: "Billing", icon: CreditCard, component: Billing }],
	databaseTabs: [{ key: "quota", label: "Quota", icon: <Gauge size={14} />, component: Quota }],
	banners: [PlanBanner],
});
