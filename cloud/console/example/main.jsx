// A cloud app that is its own shell: its header, org switcher and navigation,
// the console's cloud pages, and a database opening in DatabaseWorkspace. It
// uses only the package's exports, and no part of the local studio.
import { useState } from "react";
import { createRoot } from "react-dom/client";
import "@samyfodil/musql-console/style.css";
import { ConsoleProvider, Databases, DatabaseWorkspace, Groups, MenuButton, Members, OrgSettings, PlatformTokens, Spinner, Usage, Users, useCloud } from "@samyfodil/musql-console";
import "./shell.css";

const PAGES = [
	["databases", "Databases", Databases],
	["groups", "Groups", Groups],
	["members", "Members", Members],
	["usage", "Usage", Usage],
	["tokens", "Tokens", PlatformTokens],
	["settings", "Settings", OrgSettings],
	["users", "Users", Users, true],
];

function SignIn() {
	const cloud = useCloud();
	return (
		<div className="shell-signin">
			<div className="shell-logo">ex</div>
			<h1>Example cloud</h1>
			{cloud.auth.status === "starting" ? <Spinner label="Signing in…" /> : (
				<>
					{cloud.auth.error && <p className="err small">{cloud.auth.error}</p>}
					<button className="primary" onClick={cloud.signIn}>Sign in</button>
				</>
			)}
		</div>
	);
}

function Shell() {
	const cloud = useCloud();
	const [page, setPage] = useState("databases");
	const [open, setOpen] = useState(null); // {org, database}
	if (cloud.auth.status !== "in" || !cloud.me) return <SignIn />;
	const org = cloud.orgs.find((o) => o.slug === cloud.org);
	const go = (key) => {
		setPage(key);
		setOpen(null);
	};
	const Page = PAGES.find(([k]) => k === page)?.[2] ?? Databases;
	return (
		<div className="shell">
			<header className="shell-head">
				<div className="shell-logo">ex</div>
				<b>Example cloud</b>
				<MenuButton
					className="shell-org"
					label={<>{org?.name || cloud.org || "No organization"} <span className="dim">▾</span></>}
					items={cloud.orgs.map((o) => ({ label: o.name || o.slug, hint: o.slug, onClick: () => { cloud.setOrg(o.slug); setOpen(null); } }))}
				/>
				<span className="grow" />
				<span className="dim small">{cloud.user?.email}</span>
				<button className="ghost" onClick={cloud.signOut}>Sign out</button>
			</header>
			<nav className="shell-nav">
				{PAGES.filter(([, , , admin]) => !admin || cloud.admin).map(([k, label]) => (
					<button key={k} className={!open && page === k ? "on" : ""} onClick={() => go(k)}>{label}</button>
				))}
				{open && (
					<>
						<h4>Open</h4>
						<button className="on mono">{open.database}</button>
					</>
				)}
			</nav>
			<main className="shell-main">
				{open ? (
					<DatabaseWorkspace key={`${open.org}/${open.database}`} org={open.org} database={open.database} />
				) : (
					<div className="shell-page">
						<Page org={cloud.org} onOpenDatabase={({ org, database }) => setOpen({ org, database })} onNavigate={go} current={open} />
					</div>
				)}
			</main>
		</div>
	);
}

createRoot(document.getElementById("app")).render(
	<ConsoleProvider
		branding={{ name: "Example cloud" }}
		theme={{
			light: { accent: "#0b7285", "accent-soft": "#e3fafc", sel: "#e6fcf5" },
			dark: { accent: "#3bc9db", "accent-soft": "#0c2e33", sel: "#10292c" },
		}}
	>
		<Shell />
	</ConsoleProvider>,
);
